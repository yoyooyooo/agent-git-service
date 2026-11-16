//go:build linux

package routebstage0

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type supervisor struct {
	loaded        *loaded
	platform      platformFacts
	parent        ProcessIdentity
	child         ProcessIdentity
	loaderPath    string
	loaderSHA     string
	context       Context
	ready         chan struct{}
	mu            sync.Mutex
	verified      bool
	fatal         error
	stopped       *RuntimeIdentity
	restoreSealed bool
}

func (s *supervisor) recheck() error {
	l := s.loaded
	if err := l.recheckFiles(); err != nil {
		return err
	}
	fresh, err := currentPlatformFacts()
	if err := l.verifyAccountResources(); err != nil {
		return err
	}
	if err != nil || fresh != s.platform {
		return refused("stage0_platform_changed")
	}
	parent, err := processIdentity(os.Getpid(), l.registration.NativeImage.Path, l.registration.NativeImage.SHA256)
	if err != nil || parent != s.parent || parent.ParentPID != 1 {
		return refused("stage0_supervisor_changed")
	}
	child, err := processIdentity(s.child.PID, s.loaderPath, s.loaderSHA)
	if err != nil || child != s.child || child.ParentPID != s.parent.PID || child.UID != l.host.UID || child.GID != l.host.GID {
		return refused("stage0_child_changed")
	}
	if err = verifyChildNamespaces(child.PID, s.platform); err != nil {
		return err
	}
	if err = verifyMappings(child.PID, l.catalog); err != nil {
		return err
	}
	if err = l.verifyUnit(l.invocation, true); err != nil {
		return err
	}
	if l.invocation.Phase == "serve-old" {
		if _, err = s.restoreResult(); err != nil {
			return err
		}
	}
	return nil
}

func peer(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, refused("stage0_peer_unknown")
	}
	var credential *unix.Ucred
	var controlErr error
	err = raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil || controlErr != nil || credential == nil {
		return nil, refused("stage0_peer_unknown")
	}
	return credential, nil
}

func (s *supervisor) serveConnection(conn *net.UnixConn) {
	defer conn.Close()
	credential, err := peer(conn)
	<-s.ready
	if err != nil || int(credential.Pid) != s.child.PID || credential.Uid != s.child.UID || credential.Gid != s.child.GID {
		json.NewEncoder(conn).Encode(Response{Schema: Protocol, OK: false, Error: "stage0_peer_not_spawned_child"})
		return
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, err := bufio.NewReader(io.LimitReader(conn, 65537)).ReadBytes('\n')
	var request Request
	if err != nil || len(raw) > 65536 || decodeExact(raw, &request) != nil || request.Schema != Protocol ||
		(request.Method != "runtime_exit" && request.InvocationID != "") || (request.Method != "writer_probe" && request.Generation != "") ||
		(request.Method != "restore_complete" && request.ResultSHA256 != "") {
		json.NewEncoder(conn).Encode(Response{Schema: Protocol, OK: false, Error: "stage0_request_invalid"})
		return
	}
	response := Response{Schema: Protocol, OK: false}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		response.Error = "stage0_context_closed"
	} else if err = s.recheck(); err != nil {
		s.fatal = err
		response.Error = errorCode(err)
	} else {
		if !s.verified {
			if err = writeExclusive(s.loaded.statePath("active", s.loaded.invocation.ID), s.context); err != nil {
				s.fatal = err
				response.Error = errorCode(err)
			} else if err = s.recordManagerStart(); err != nil {
				s.fatal = err
				response.Error = errorCode(err)
			} else {
				s.verified = true
			}
		}
		if response.Error == "" {
			response = s.dispatch(request)
			if response.OK && request.Method != "recheck" {
				if err = s.recheck(); err != nil {
					s.fatal = err
					response = Response{Schema: Protocol, Error: errorCode(err)}
				}
			}
		}
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	json.NewEncoder(conn).Encode(response)
}

func (s *supervisor) dispatch(request Request) Response {
	response := Response{Schema: Protocol, OK: true, Context: &s.context}
	switch request.Method {
	case "recheck":
	case "runtime_identity":
		value, err := s.runtimeIdentity()
		if err != nil {
			return Response{Schema: Protocol, Error: errorCode(err)}
		}
		response.Runtime = &value
	case "runtime_exit":
		value, completion, err := s.runtimeExitEvidence(request.InvocationID)
		if err != nil {
			return Response{Schema: Protocol, Error: errorCode(err)}
		}
		response.Exit = &value
		response.ManagerCompletion = &completion
	case "manager_stop", "manager_start":
		if err := s.managerAction(request.Method); err != nil {
			return Response{Schema: Protocol, Error: errorCode(err)}
		}
		response.Action = request.Method
	case "writer_probe":
		value, err := s.writerProbe(request.Generation)
		if err != nil {
			return Response{Schema: Protocol, Error: errorCode(err)}
		}
		response.Writers = &value
	case "restore_complete":
		value, err := s.completeRestore(request.ResultSHA256)
		if err != nil {
			return Response{Schema: Protocol, Error: errorCode(err)}
		}
		response.Restore = &value
	case "restore_result":
		value, err := s.restoreResult()
		if err != nil {
			return Response{Schema: Protocol, Error: errorCode(err)}
		}
		response.Restore = &value
	default:
		return Response{Schema: Protocol, Error: "stage0_method_invalid"}
	}
	return response
}

func Run(id, operation string) error {
	l, err := load(id)
	if err != nil {
		return err
	}
	if operation == "restore" || operation == "old-start" {
		if err = l.loadRecovery(operation); err != nil {
			return err
		}
	}
	platform, parent, err := platformPrerequisites(l)
	if err != nil {
		return err
	}
	// Select the fixed manager unit before consuming any operation slot.
	var unit *Invocation
	for _, i := range l.registration.Invocations {
		if i.Operation == operation {
			copy := i
			unit = &copy
			break
		}
	}
	if unit == nil {
		return refused("stage0_operation_unregistered")
	}
	if err = l.verifyUnit(*unit, true); err != nil {
		return err
	}
	python, err := childArguments(l.registration, l.host, l.catalog.Evidence().Executable, operation)
	if err != nil {
		return err
	}
	loader, err := l.catalog.LinuxInvocation()
	if err != nil {
		return err
	}
	file := l.catalog.Files()[loader[0]]
	if file.Path == "" {
		return refused("stage0_loader_unregistered")
	}
	if err = l.claim(operation, parent, platform.boot); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath(os.Getpid()), Net: "unix"})
	if err != nil {
		return refused("stage0_socket_unavailable")
	}
	defer listener.Close()
	if err = os.Chmod(socketPath(os.Getpid()), 0600); err != nil {
		return refused("stage0_socket_permissions")
	}
	if err = os.Chown(socketPath(os.Getpid()), int(l.host.UID), int(l.host.GID)); err != nil {
		return refused("stage0_socket_permissions")
	}
	command := exec.Command(loader[0], append(loader[1:], python[1:]...)...)
	command.Dir = l.host.ToolBundle
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "HOME=" + l.host.Home, "USER=" + l.host.User, "LOGNAME=" + l.host.User}
	command.SysProcAttr = childProcessAttributes(l.host.UID, l.host.GID)
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err = command.Start(); err != nil {
		return refused("stage0_child_start_failed")
	}
	s := &supervisor{loaded: l, platform: platform, parent: parent, loaderPath: loader[0], loaderSHA: file.SHA256, ready: make(chan struct{})}
	// A child can connect immediately, but authentication waits for its exact
	// post-exec image/birth. No parent argv or caller PID supplies that identity.
	deadline := time.Now().Add(5 * time.Second)
	identityObserved := false
	for time.Now().Before(deadline) {
		s.child, err = processIdentity(command.Process.Pid, loader[0], file.SHA256)
		if err == nil && s.child.ParentPID == parent.PID && s.child.UID == l.host.UID && s.child.GID == l.host.GID {
			identityObserved = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !identityObserved {
		// The invocation claim remains consumed even if recording fails. Do
		// not accept a peer or return while this unverified child is alive.
		failure := map[string]any{"schema": "ags.routeb-stage0-identity-unavailable.v1",
			"registration_sha256": l.registrationSHA, "invocation_id": l.invocation.ID,
			"boot_id": platform.boot, "child_pid": command.Process.Pid,
			"reason": "stage0_child_identity_unavailable", "retry_allowed": false}
		recordErr := writeExclusive(l.statePath("failures", l.invocation.ID), failure)
		fmt.Fprintln(os.Stderr, "code=stage0_child_identity_unavailable child_exit=unobserved retry_allowed=false")
		waitErr := stopSpawnedChild(command)
		failure["child_exit_observed"] = command.ProcessState != nil
		failure["wait_succeeded"] = waitErr == nil
		failure["child_exit_code"] = -1
		if command.ProcessState != nil {
			failure["child_exit_code"] = command.ProcessState.ExitCode()
		}
		exitRecordErr := writeExclusive(l.statePath("failure-exits", l.invocation.ID), failure)
		if recordErr != nil || exitRecordErr != nil {
			return refused("stage0_identity_failure_record_unavailable")
		}
		return refused("stage0_child_identity_unavailable")
	}
	i := l.invocation
	b := l.registration.Binding
	grantSHA := ""
	if l.registration.ScopeGrant != nil {
		grantSHA = l.registration.ScopeGrant.SHA256
	}
	s.context = Context{"ags.routeb-stage0-context.v1", l.registration.ID, l.registrationSHA, l.host.Root, operation, i.Phase, i.ID, i.AttemptID, i.AuthorityEpoch, i.AuthorizationID, platform.boot, parent, s.child, l.registration.Interpreter.SHA256, b.ToolRevision, b.ToolTree, b.ToolManifestSHA256, l.recoverySHA, grantSHA}
	close(s.ready)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			connection, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			go s.serveConnection(connection)
		}
	}()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	stopSignals := make(chan struct{})
	var forwarded atomic.Bool
	go func() {
		for {
			select {
			case <-signals:
				if !forwarded.Load() {
					if command.Process.Signal(syscall.SIGTERM) == nil {
						forwarded.Store(true)
					}
				}
			case <-stopSignals:
				return
			}
		}
	}()
	waitErr := command.Wait()
	close(stopSignals)
	listener.Close()
	<-acceptDone
	s.mu.Lock()
	defer s.mu.Unlock()
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	recheckErr := l.recheckFiles()
	group, groupErr := cgroupMembers(i.Unit)
	groupDrained := groupErr == nil && len(group) == 1 && group[0] == os.Getpid()
	normal := waitErr == nil && exitCode == 0 && s.verified && s.fatal == nil && recheckErr == nil && groupDrained && (operation != "serve" || forwarded.Load()) && (operation != "restore" || s.restoreSealed)
	record := ExitRecord{"ags.routeb-stage0-exit.v1", l.registrationSHA, i.ID, i.Phase, platform.boot, parent, s.child, exitCode, forwarded.Load(), false, s.verified, recheckErr == nil, normal}
	if err = writeExclusive(l.statePath("exits", i.ID), record); err != nil {
		return err
	}
	if !normal {
		return refused("stage0_child_or_authority_failed")
	}
	return nil
}
