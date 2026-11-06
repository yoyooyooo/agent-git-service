//go:build linux

package routebstage0

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDescriptorHeaderRequiresIdentityAndOctalAccess(t *testing.T) {
	s := kernelScan{}
	info, err := s.descriptorReader(strings.NewReader("pos:\t0\nflags:\t0100002\nmnt_id:\t24\nino:\t1234\n"))
	if err != nil || info.inode != 1234 || info.mount != 24 || info.flags&unix.O_ACCMODE != unix.O_RDWR {
		t.Fatal(info, err)
	}
	for _, raw := range []string{
		"flags: 0100002\nmnt_id: 24\n", "flags: 0100008\nmnt_id: 24\nino: 1234\n",
		"flags: 0\nflags: 2\nmnt_id: 24\nino: 1234\n", "flags: 0\nmnt_id: -1\nino: 1234\n",
		strings.Repeat("x", 65537),
	} {
		if _, err := s.descriptorReader(strings.NewReader(raw)); err == nil {
			t.Fatal("incomplete descriptor accepted")
		}
	}
}

func TestMountInventoryDoesNotResolveForeignFilesystemPaths(t *testing.T) {
	raw := []byte("24 1 8:2 / / rw - ext4 /dev/sda2 rw\n900 24 0:55 / /unreadable/fuse\\040mount rw - fuse.portal portal rw\n")
	devices, err := mountDevices(raw)
	if err != nil || devices[24] != unix.Mkdev(8, 2) || devices[900] != unix.Mkdev(0, 55) {
		t.Fatal(devices, err)
	}
	for _, bad := range []string{string(raw) + "24 1 8:3 / /other rw - ext4 none rw\n", "24 1 unknown / / rw - ext4 none rw\n"} {
		if _, err := mountDevices([]byte(bad)); err == nil {
			t.Fatal("ambiguous device accepted")
		}
	}
}

func TestWritableSharedMappingRemainsAWriterWithoutDescriptor(t *testing.T) {
	key := fileKey{unix.Mkdev(8, 2), 1234}
	s := kernelScan{keys: map[fileKey]map[string]bool{key: {"sqlite": true}}, inodes: map[uint64]bool{1234: true}, counts: map[string]int{}}
	for _, line := range []string{
		"1000-2000 rw-p 00000000 08:02 1234 /target\n",
		"1000-2000 r--s 00000000 08:02 1234 /target\n",
		"1000-2000 rw-s 00000000 08:03 1234 /unrelated\n",
	} {
		if err := s.inspectMappings([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.counts) != 0 {
		t.Fatal("private, readonly, or different-device map treated as writer")
	}
	if err := s.inspectMappings([]byte("1000-2000 rw-s 00000000 08:02 1234 /target (deleted)\n")); err != nil || s.counts["sqlite"] != 1 {
		t.Fatal(s.counts, err)
	}
	if err := s.inspectMappings([]byte("1000-2000 rw-s 00000000 invalid 1234 /target\n")); err == nil {
		t.Fatal("matching inode with unknown device accepted")
	}
}

func holdTestWriterTarget(t *testing.T, path string, directory bool) heldWriterTarget {
	t.Helper()
	fd, err := openWriterPath(unix.AT_FDCWD, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	stamp, err := writerStat(fd)
	if err != nil {
		t.Fatal(err)
	}
	h := heldWriterTarget{target: writerTarget{id: "fixture", path: path, directory: directory}, fd: fd, stamp: stamp, nodes: map[string]writerStamp{}}
	if err = collectWriterNodes(fd, ".", h.nodes); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHeldTargetRecheckRejectsPathReplacementAndChangedMembers(t *testing.T) {
	t.Run("replacement", func(t *testing.T) {
		base := t.TempDir()
		path := filepath.Join(base, "object")
		if err := os.WriteFile(path, []byte("same bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		h := holdTestWriterTarget(t, path, false)
		if err := recheckWriterTarget(h); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, path+".original"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("same bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := recheckWriterTarget(h); err == nil {
			t.Fatal("replacement path admitted because old fd stayed open")
		}
	})
	t.Run("member", func(t *testing.T) {
		path := t.TempDir()
		h := holdTestWriterTarget(t, path, true)
		if err := os.WriteFile(filepath.Join(path, "late"), []byte("new"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := recheckWriterTarget(h); err == nil {
			t.Fatal("changed directory admitted")
		}
	})
	t.Run("optional-wal", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "database-wal")
		h := heldWriterTarget{target: writerTarget{id: "db", path: path, optional: true}, fd: -1, absent: true}
		if err := recheckWriterTarget(h); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("wal"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := recheckWriterTarget(h); err == nil {
			t.Fatal("appeared SQLite sidecar admitted")
		}
	})
}

func TestWriterTreeRejectsAliasesAndUnsupportedMembers(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "member")
			if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("member", filepath.Join(dir, "alias"))
			case "hardlink":
				err = os.Link(path, filepath.Join(dir, "alias"))
			case "fifo":
				err = unix.Mkfifo(filepath.Join(dir, "fifo"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			fd, err := openWriterPath(unix.AT_FDCWD, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			if err = collectWriterNodes(fd, ".", map[string]writerStamp{}); err == nil {
				t.Fatal("unsupported member admitted")
			}
		})
	}
}

func TestKernelObserverBudgetsAndZeroBootTicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, scan := range []kernelScan{{bytes: maxWriterBytes}, {deadline: time.Now().Add(-time.Second)}} {
		if _, err := scan.read(path, 4); err == nil {
			t.Fatal("read budget ignored")
		}
	}
	raw := []byte("2 (kthreadd) S 0 " + strings.Repeat("0 ", 17) + "0 0")
	if birth, zombie, err := writerBirth(raw); err != nil || birth != 0 || zombie {
		t.Fatal(birth, zombie, err)
	}
	if _, zombie, err := writerBirth([]byte(strings.Replace(string(raw), ") S ", ") Z ", 1))); err != nil || !zombie {
		t.Fatal(zombie, err)
	}
}

func TestIdentityFailureReapsOnlyItsOwnChildWithinBound(t *testing.T) {
	if os.Getenv("AGS_STAGE0_TEST_IGNORE_TERM") == "1" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		fmt.Println("ready")
		for {
			<-signals
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestIdentityFailureReapsOnlyItsOwnChildWithinBound$", "-test.count=1")
	command.Env = append(os.Environ(), "AGS_STAGE0_TEST_IGNORE_TERM=1")
	pipe, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(pipe).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatal(line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child not ready")
	}
	start := time.Now()
	forced := stopSpawnedChild(command, 30*time.Millisecond)
	if !forced || time.Since(start) > 2*time.Second || command.ProcessState == nil || command.ProcessState.Success() {
		t.Fatal("unbounded or falsely normal reap", forced, command.ProcessState)
	}
}
