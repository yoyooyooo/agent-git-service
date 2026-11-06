//go:build linux && routeb_native_probe

package routebstage0

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const nativeProbeRoot = "/root/ags-routeb-kernel-observer-fixture"

// This helper is present only in the explicitly tagged probe binary. It has
// no registration, manager, production path, or caller-selected command.
func TestNativeKernelWriterChild(t *testing.T) {
	mode := os.Getenv("AGS_STAGE0_NATIVE_PROBE_CHILD")
	if mode == "" {
		return
	}
	if mode != "readonly" && mode != "writeonly" && mode != "readwrite" && mode != "shared" && mode != "private" {
		t.Fatal("unknown fixed child mode")
	}
	var executable, actual unix.Stat_t
	if unix.Fstat(4, &executable) != nil || unix.Stat("/proc/self/exe", &actual) != nil || executable.Dev != actual.Dev || executable.Ino != actual.Ino {
		t.Fatal("exec fd is not the actual image")
	}
	if err := unix.Close(4); err != nil {
		t.Fatal(err)
	}
	var mapping []byte
	if mode == "shared" || mode == "private" {
		flags := unix.MAP_SHARED
		if mode == "private" {
			flags = unix.MAP_PRIVATE
		}
		var err error
		mapping, err = unix.Mmap(3, 0, 4096, unix.PROT_READ|unix.PROT_WRITE, flags)
		if err != nil {
			t.Fatal(err)
		}
		if err = unix.Close(3); err != nil {
			t.Fatal(err)
		}
		defer unix.Munmap(mapping)
	} else {
		defer unix.Close(3)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true, "pid": os.Getpid(), "mode": mode}); err != nil {
		t.Fatal(err)
	}
	released := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(os.Stdin).ReadString('\n'); released <- line }()
	select {
	case line := <-released:
		if line != "release\n" {
			t.Fatal("child release unavailable")
		}
	case <-time.After(45 * time.Second):
		t.Fatal("bounded child release deadline")
	}
	runtime.KeepAlive(mapping)
}

type nativeCase struct {
	name, mode                                  string
	uid                                         uint32
	directory, unrelated, exitFirst, wantWriter bool
}

func nativeHolder(t *testing.T, path, mode string, uid uint32) (int, func(), map[string]any) {
	t.Helper()
	flags := os.O_RDONLY
	if mode == "writeonly" {
		flags = os.O_WRONLY
	} else if mode != "readonly" {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer executable.Close()
	// The protected directory stays 0700. Only the fixed read-only image fd
	// and this case's data fd cross the deliberately selected uid boundary.
	command := exec.Command("/proc/self/fd/4", "-test.run=^TestNativeKernelWriterChild$", "-test.count=1", "-test.timeout=50s")
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC", "GOMAXPROCS=2", "AGS_STAGE0_NATIVE_PROBE_CHILD=" + mode}
	command.ExtraFiles = []*os.File{file, executable}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{}}, Pdeathsig: syscall.SIGKILL}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			stopSpawnedChild(command, 100*time.Millisecond)
		}
	})
	ready := make(chan []byte, 1)
	go func() { line, _ := bufio.NewReader(output).ReadBytes('\n'); ready <- line }()
	select {
	case line := <-ready:
		var value struct {
			Ready bool   `json:"ready"`
			PID   int    `json:"pid"`
			Mode  string `json:"mode"`
		}
		if err = json.Unmarshal(line, &value); err != nil || !value.Ready || value.PID != command.Process.Pid || value.Mode != mode {
			t.Fatalf("child not ready: %s %s", line, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded ready deadline")
	}
	// Parent writers are closed before the census, including mmap cases.
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = executable.Close(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	status, err := procRead(pid, "status", 65536)
	if err != nil {
		t.Fatal(err)
	}
	actualUID, actualGID, err := statusCredentials(status)
	if err != nil || actualUID != uid || actualGID != uid {
		t.Fatal("actual holder credentials differ", err)
	}
	if _, err = privilegePolicy(status, uid == 0); err != nil {
		t.Fatal(err)
	}
	stat, err := procRead(pid, "stat", 65536)
	if err != nil {
		t.Fatal(err)
	}
	parent, birth, err := statBirth(stat)
	if err != nil || parent != os.Getpid() {
		t.Fatal("actual holder birth differs", err)
	}
	var actualImage, selfImage unix.Stat_t
	if unix.Stat(filepath.Join("/proc", decimal(pid), "exe"), &actualImage) != nil || unix.Stat("/proc/self/exe", &selfImage) != nil || actualImage.Dev != selfImage.Dev || actualImage.Ino != selfImage.Ino {
		t.Fatal("actual helper image differs")
	}
	proof := map[string]any{"pid": pid, "start_ticks": birth, "uid": actualUID, "gid": actualGID, "actual_image_inode": actualImage.Ino, "actual_image_device": actualImage.Dev, "mode": mode}
	closed := false
	release := func() {
		if closed {
			return
		}
		closed = true
		fmt.Fprintln(input, "release")
		input.Close()
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case err := <-done:
			reaped = true
			if err != nil {
				t.Fatalf("helper did not exit normally: %v %s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			command.Process.Kill()
			select {
			case <-done:
				reaped = true
			case <-time.After(5 * time.Second):
			}
			t.Fatal("bounded helper exit deadline")
		}
	}
	return pid, release, proof
}

func actualHolderObjects(t *testing.T, pid int, path string) ([]uint64, []string) {
	t.Helper()
	var target unix.Stat_t
	if unix.Stat(path, &target) != nil {
		t.Fatal("target metadata unavailable")
	}
	s := kernelScan{deadline: time.Now().Add(5 * time.Second)}
	raw, err := s.read(filepath.Join("/proc", decimal(pid), "mountinfo"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := mountDevices(raw)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join("/proc", decimal(pid), "fd"))
	if err != nil {
		t.Fatal(err)
	}
	access := []uint64{}
	for _, entry := range entries {
		info, err := s.descriptor(filepath.Join("/proc", decimal(pid), "fdinfo", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if info.inode == target.Ino && devices[info.mount] == uint64(target.Dev) {
			access = append(access, info.flags&unix.O_ACCMODE)
		}
	}
	raw, err = s.read(filepath.Join("/proc", decimal(pid), "maps"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	mappings := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 5 {
			continue
		}
		ino, e := strconv.ParseUint(parts[4], 10, 64)
		if e != nil || ino != target.Ino {
			continue
		}
		pair := strings.Split(parts[3], ":")
		if len(pair) != 2 {
			t.Fatal("unknown map device")
		}
		major, e1 := strconv.ParseUint(pair[0], 16, 32)
		minor, e2 := strconv.ParseUint(pair[1], 16, 32)
		if e1 != nil || e2 != nil {
			t.Fatal("unknown map device")
		}
		if unix.Mkdev(uint32(major), uint32(minor)) == uint64(target.Dev) {
			mappings = append(mappings, parts[1])
		}
	}
	return access, mappings
}

func TestNativeKernelWriterProbe(t *testing.T) {
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Fatal("explicit root fixture required")
	}
	info, err := os.Lstat(nativeProbeRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Fatal("fixed protected probe root required")
	}
	facts, err := currentPlatformFacts()
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := os.MkdirTemp(nativeProbeRoot, "attempt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("evidence=%s boot=%s net_namespace=%s native_admission=false", attempt, facts.boot, facts.netNamespace)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	s := kernelScan{}
	if count, err := s.listeners(port); err != nil || count != 1 {
		listener.Close()
		t.Fatal("actual own listener not observed", count, err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	cases := []nativeCase{
		{name: "clean-readonly-directory", directory: true},
		{name: "root-writeonly-file", mode: "writeonly", wantWriter: true},
		{name: "user-readwrite-member", mode: "readwrite", uid: 1000, directory: true, wantWriter: true},
		{name: "closed-fd-shared-map", mode: "shared", uid: 1000, wantWriter: true},
		{name: "closed-fd-private-map", mode: "private", uid: 1000},
		{name: "user-readonly-descriptor", mode: "readonly", uid: 1000},
		{name: "unrelated-writer", mode: "readwrite", uid: 1000, unrelated: true},
		{name: "exited-holder-fresh-clean", mode: "readwrite", uid: 1000, exitFirst: true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			caseDir := filepath.Join(attempt, item.name)
			if err := os.Mkdir(caseDir, 0700); err != nil {
				t.Fatal(err)
			}
			record := map[string]any{"schema": "ags.routeb-kernel-observer-probe.v1", "case": item.name, "native_admission": false, "expected_writer": item.wantWriter}
			t.Cleanup(func() {
				record["passed"] = !t.Failed()
				raw, e := json.MarshalIndent(record, "", "  ")
				if e == nil {
					e = os.WriteFile(filepath.Join(caseDir, "result.json"), append(raw, '\n'), 0600)
				}
				if e != nil {
					t.Error(e)
				}
			})
			targetDir := filepath.Join(caseDir, "objects")
			if err := os.Mkdir(targetDir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(targetDir, "member")
			if err := os.WriteFile(path, make([]byte, 4096), 0600); err != nil {
				t.Fatal(err)
			}
			target := path
			if item.directory {
				target = targetDir
			}
			holderPath := path
			if item.unrelated {
				holderPath = filepath.Join(caseDir, "unrelated")
				if err := os.WriteFile(holderPath, make([]byte, 4096), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if item.mode != "" {
				pid, release, proof := nativeHolder(t, holderPath, item.mode, item.uid)
				defer release()
				access, mappings := actualHolderObjects(t, pid, holderPath)
				proof["actual_access_modes"] = access
				proof["actual_mapping_permissions"] = mappings
				record["holder"] = proof
				switch item.mode {
				case "shared", "private":
					expected := "rw-s"
					if item.mode == "private" {
						expected = "rw-p"
					}
					if len(access) != 0 || len(mappings) != 1 || mappings[0] != expected {
						t.Fatal("mmap case did not have closed descriptors and the exact actual mapping", access, mappings)
					}
				default:
					want := uint64(unix.O_RDWR)
					if item.mode == "readonly" {
						want = unix.O_RDONLY
					} else if item.mode == "writeonly" {
						want = unix.O_WRONLY
					}
					if len(access) != 1 || access[0] != want {
						t.Fatal("descriptor case not established", access)
					}
				}
				if item.exitFirst {
					release()
					if !writerGone(pid) {
						t.Fatal("holder still exists")
					}
				}
			}
			started := time.Now()
			writers, err := kernelWriters([]writerTarget{{id: "fixture", path: target, directory: item.directory}}, port)
			record["duration_ms"] = time.Since(started).Milliseconds()
			record["writers"] = writers
			if err != nil {
				record["error"] = ErrorCode(err)
				t.Fatal(err)
			}
			if (len(writers) > 0) != item.wantWriter {
				t.Fatalf("unexpected census: %#v", writers)
			}
			for _, writer := range writers {
				if writer.Kind != "holder" || writer.Object != "fixture" || writer.Count <= 0 {
					t.Fatal("unrelated result", writer)
				}
			}
			if after, err := currentPlatformFacts(); err != nil || after != facts {
				t.Fatal("platform changed", err)
			}
			t.Logf("case=%s duration_ms=%v writers=%d", item.name, record["duration_ms"], len(writers))
		})
	}
}
