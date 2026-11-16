//go:build linux

package routebstage0

import (
	"crypto/rand"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestKernelPeerRejectsAnotherProcessWithSameUID(t *testing.T) {
	// The helper is an ordinary process of the same test user. Its JSON claims
	// the parent's identity, but the server authenticates the kernel PID first.
	if path := os.Getenv("AGS_STAGE0_TEST_PEER_SOCKET"); path != "" {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		json.NewEncoder(conn).Encode(map[string]any{"schema": Protocol, "method": "recheck", "claimed_parent": os.Getppid(), "verified": true})
		var value Response
		if err = json.NewDecoder(conn).Decode(&value); err != nil {
			t.Fatal(err)
		}
		if value.OK || value.Error != "stage0_peer_not_spawned_child" {
			t.Fatalf("impostor admitted: %#v", value)
		}
		return
	}
	// This Linux-specific test proves kernel peer credentials, independently
	// of a runner's TMPDIR length. The random abstract address creates no
	// filesystem entry and stays within sockaddr_un's fixed name budget.
	path := "@ags-stage0-peer-" + rand.Text()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s := &supervisor{child: ProcessIdentity{PID: os.Getpid(), UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}, ready: make(chan struct{})}
	close(s.ready)
	done := make(chan error, 1)
	go func() {
		connection, e := listener.AcceptUnix()
		if e == nil {
			s.serveConnection(connection)
		}
		done <- e
	}()
	command := exec.Command(os.Args[0], "-test.run=^TestKernelPeerRejectsAnotherProcessWithSameUID$", "-test.count=1")
	command.Env = append(os.Environ(), "AGS_STAGE0_TEST_PEER_SOCKET="+path)
	if raw, e := command.CombinedOutput(); e != nil {
		t.Fatalf("peer negative failed: %v %s", e, raw)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCredentialAndBirthParsingRejectsUnknownIdentity(t *testing.T) {
	if uid, gid, err := statusCredentials([]byte("Uid:\t1000 1000 1000 1000\nGid:\t1000 1000 1000 1000\n")); err != nil || uid != 1000 || gid != 1000 {
		t.Fatal(uid, gid, err)
	}
	for _, raw := range []string{"Uid: 1000 0 1000 1000\nGid: 1000 1000 1000 1000\n", "Uid: 1000 1000 1000 1000\n", "Uid: 1000 1000 1000 1000\nUid: 1000 1000 1000 1000\nGid: 1000 1000 1000 1000\n"} {
		if _, _, err := statusCredentials([]byte(raw)); err == nil {
			t.Fatal("ambiguous credentials accepted")
		}
	}
	raw := []byte("99 (name with ) spaces) S 1 " + strings.Repeat("0 ", 17) + "123456 0")
	if parent, birth, err := statBirth(raw); err != nil || parent != 1 || birth != 123456 {
		t.Fatal(parent, birth, err)
	}
	if _, _, err := statBirth([]byte(strings.Replace(string(raw), ") S ", ") Z ", 1))); err == nil {
		t.Fatal("zombie treated as live")
	}
}

func TestNSSAuthorizationUsesOnlyTheFrozenFirstFilesMatch(t *testing.T) {
	h := HostProfile{UID: 1000, GID: 1000, User: "fixture", Home: "/home/example"}
	passwd := []byte("root:x:0:0:root:/root:/bin/bash\nfixture:x:1000:1000::/home/example:/bin/bash\n")
	if err := accountResourcePolicy(passwd, []byte("passwd: files systemd\ngroup: files\n"), h); err != nil {
		t.Fatal(err)
	}
	for _, nss := range []string{"passwd: systemd files", "passwd: files [SUCCESS=continue] systemd", "passwd: files\npasswd: files", "group: files"} {
		if err := accountResourcePolicy(passwd, []byte(nss), h); err == nil {
			t.Fatal("unmodeled NSS lookup accepted")
		}
	}
	for _, bad := range [][]byte{append(append([]byte{}, passwd...), []byte("another:x:1000:1000::/home/user:/bin/bash\n")...), []byte(strings.Replace(string(passwd), "/home/example", "/tmp/other", 1))} {
		if err := accountResourcePolicy(bad, []byte("passwd: files"), h); err == nil {
			t.Fatal("ambiguous or redirected account accepted")
		}
	}
}

func TestWriterVisibilityAndUnprivilegedChildHaveDifferentCapabilityContracts(t *testing.T) {
	child := []byte("CapEff:\t0000000000000000\nCapPrm:\t0000000000000000\nCapInh:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\n")
	if _, err := privilegePolicy(child, false); err != nil {
		t.Fatal(err)
	}
	if _, err := privilegePolicy(child, true); err == nil {
		t.Fatal("UID without capabilities accepted as global observer")
	}
	root := []byte(strings.ReplaceAll(string(child), "CapEff:\t0000000000000000", "CapEff:\t0000000000080002"))
	root = []byte(strings.ReplaceAll(string(root), "CapPrm:\t0000000000000000", "CapPrm:\t0000000000080002"))
	if _, err := privilegePolicy(root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := privilegePolicy(root, false); err == nil {
		t.Fatal("privileged interpreter child accepted")
	}
	for _, bad := range []string{strings.ReplaceAll(string(root), "NoNewPrivs:\t1", "NoNewPrivs:\t0"), strings.ReplaceAll(string(root), "0000000000080002", "0000000000000002"), strings.ReplaceAll(string(root), "CapAmb:\t0000000000000000", "CapAmb:\t0000000000080002")} {
		if _, err := privilegePolicy([]byte(bad), true); err == nil {
			t.Fatal("unsupported process privilege state accepted")
		}
	}
}
