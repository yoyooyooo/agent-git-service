//go:build linux

package routebstage0

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxWriterNodes = 100000
const maxWriterProcesses = 16384
const maxWriterFDs = 262144
const maxWriterMappings = 1048576
const maxWriterBytes = 128 << 20

type fileKey struct{ device, inode uint64 }
type writerStamp struct {
	key                fileKey
	mode, uid, gid     uint32
	links              uint64
	size, mtime, ctime int64
}
type heldWriterTarget struct {
	target writerTarget
	fd     int
	stamp  writerStamp
	nodes  map[string]writerStamp
	absent bool
}
type kernelScan struct {
	bytes, fds, mappings int
	keys                 map[fileKey]map[string]bool
	inodes               map[uint64]bool
	witness              map[int]fileKey
	seenWitness          map[int]bool
	counts               map[string]int
	deadline             time.Time
}

func writerStat(fd int) (writerStamp, error) {
	var info unix.Stat_t
	if unix.Fstat(fd, &info) != nil {
		return writerStamp{}, refused("stage0_writer_target_unknown")
	}
	kind := info.Mode & unix.S_IFMT
	if (kind != unix.S_IFREG && kind != unix.S_IFDIR) || (kind == unix.S_IFREG && info.Nlink != 1) {
		return writerStamp{}, refused("stage0_writer_target_unsupported")
	}
	return writerStamp{fileKey{uint64(info.Dev), info.Ino}, info.Mode, info.Uid, info.Gid, uint64(info.Nlink), info.Size, info.Mtim.Nano(), info.Ctim.Nano()}, nil
}

func openWriterPath(base int, path string) (int, error) {
	resolve := uint64(unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS)
	if base != unix.AT_FDCWD {
		resolve |= unix.RESOLVE_NO_XDEV
	}
	return unix.Openat2(base, path, &unix.OpenHow{Flags: unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: resolve})
}

func collectWriterNodes(fd int, name string, nodes map[string]writerStamp) error {
	return collectWriterNodesAt(fd, name, nodes, 0)
}

func collectWriterNodesAt(fd int, name string, nodes map[string]writerStamp, depth int) error {
	if depth > 128 {
		return refused("stage0_writer_inventory_limit")
	}
	if len(nodes) >= maxWriterNodes {
		return refused("stage0_writer_inventory_limit")
	}
	info, err := writerStat(fd)
	if err != nil {
		return err
	}
	nodes[name] = info
	if info.mode&unix.S_IFMT != unix.S_IFDIR {
		return nil
	}
	dirFD, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return refused("stage0_writer_directory_unreadable")
	}
	dir := os.NewFile(uintptr(dirFD), name)
	defer dir.Close()
	entries, err := dir.ReadDir(maxWriterNodes + 1)
	if err != nil && err != io.EOF {
		return refused("stage0_writer_directory_unreadable")
	}
	if len(entries) > maxWriterNodes {
		return refused("stage0_writer_inventory_limit")
	}
	for _, entry := range entries {
		part := entry.Name()
		if part == "." || part == ".." || strings.ContainsAny(part, "/\x00") {
			return refused("stage0_writer_member_invalid")
		}
		child, err := openWriterPath(dirFD, part)
		if err != nil {
			return refused("stage0_writer_member_changed")
		}
		err = collectWriterNodesAt(child, filepath.Join(name, part), nodes, depth+1)
		unix.Close(child)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *kernelScan) read(path string, maximum int) ([]byte, error) {
	if !s.deadline.IsZero() && time.Now().After(s.deadline) {
		return nil, refused("stage0_writer_inventory_deadline")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(maximum)+1))
	s.bytes += len(raw)
	if err != nil {
		return nil, err
	}
	if len(raw) > maximum || s.bytes > maxWriterBytes {
		return nil, refused("stage0_writer_inventory_limit")
	}
	return raw, nil
}

func writerBirth(raw []byte) (uint64, bool, error) {
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return 0, false, refused("stage0_writer_process_unknown")
	}
	parts := strings.Fields(string(raw)[end+2:])
	if len(parts) < 20 {
		return 0, false, refused("stage0_writer_process_unknown")
	}
	birth, err := strconv.ParseUint(parts[19], 10, 64)
	if err != nil {
		return 0, false, refused("stage0_writer_process_unknown")
	}
	return birth, parts[0] == "Z" || parts[0] == "X", nil
}

type writerTask struct{ tid, tgid int }

func writerTasks() ([]writerTask, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, refused("stage0_writer_process_inventory_unknown")
	}
	ids := []writerTask{}
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid <= 0 {
			continue
		}
		tasks, e := os.ReadDir(filepath.Join("/proc", decimal(pid), "task"))
		if e != nil {
			if writerGone(pid) {
				continue
			}
			return nil, refused("stage0_writer_process_inventory_unknown")
		}
		for _, task := range tasks {
			tid, e := strconv.Atoi(task.Name())
			if e != nil || tid <= 0 {
				return nil, refused("stage0_writer_process_inventory_unknown")
			}
			ids = append(ids, writerTask{tid, pid})
			if len(ids) > maxWriterProcesses {
				return nil, refused("stage0_writer_inventory_limit")
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].tid < ids[j].tid })
	return ids, nil
}

func writerGone(pid int) bool { return syscall.Kill(pid, 0) == syscall.ESRCH }

func writerTaskGone(task writerTask) bool { return unix.Tgkill(task.tgid, task.tid, 0) == unix.ESRCH }

func writerTaskPath(task writerTask) string {
	return filepath.Join("/proc", decimal(task.tgid), "task", decimal(task.tid))
}

func mountDevices(raw []byte) (map[uint64]uint64, error) {
	values := map[uint64]uint64{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 7 {
			return nil, refused("stage0_writer_mount_inventory_unknown")
		}
		id, e1 := strconv.ParseUint(parts[0], 10, 64)
		device := strings.Split(parts[2], ":")
		if e1 != nil || len(device) != 2 || id == 0 {
			return nil, refused("stage0_writer_mount_inventory_unknown")
		}
		major, e1 := strconv.ParseUint(device[0], 10, 32)
		minor, e2 := strconv.ParseUint(device[1], 10, 32)
		if _, exists := values[id]; exists || e1 != nil || e2 != nil {
			return nil, refused("stage0_writer_mount_inventory_unknown")
		}
		values[id] = unix.Mkdev(uint32(major), uint32(minor))
	}
	return values, nil
}

type descriptorInfo struct{ inode, mount, flags uint64 }

func (s *kernelScan) descriptor(path string) (descriptorInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return descriptorInfo{}, err
	}
	defer f.Close()
	return s.descriptorReader(f)
}

func (s *kernelScan) descriptorReader(reader io.Reader) (descriptorInfo, error) {
	if !s.deadline.IsZero() && time.Now().After(s.deadline) {
		return descriptorInfo{}, refused("stage0_writer_inventory_deadline")
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, 65537))
	scanner.Buffer(make([]byte, 4096), 65536)
	values := map[string]uint64{}
	for scanner.Scan() {
		line := scanner.Text()
		s.bytes += len(line) + 1
		if s.bytes > maxWriterBytes {
			return descriptorInfo{}, refused("stage0_writer_inventory_limit")
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || (key != "flags" && key != "mnt_id" && key != "ino") {
			continue
		}
		if _, exists := values[key]; exists {
			return descriptorInfo{}, refused("stage0_writer_descriptor_unknown")
		}
		base := 10
		if key == "flags" {
			base = 8
		}
		n, e := strconv.ParseUint(strings.TrimSpace(value), base, 64)
		if e != nil {
			return descriptorInfo{}, refused("stage0_writer_descriptor_unknown")
		}
		values[key] = n
		// Linux emits these three header fields before type-specific epoll or
		// io_uring details. Those details cannot alter this file's identity.
		if len(values) == 3 {
			return descriptorInfo{values["ino"], values["mnt_id"], values["flags"]}, nil
		}
	}
	return descriptorInfo{}, refused("stage0_writer_descriptor_unknown")
}

func (s *kernelScan) add(key fileKey) {
	for id := range s.keys[key] {
		s.counts[id]++
	}
}

type writerObservation struct {
	birth    uint64
	mappings bool
}

func (s *kernelScan) process(task writerTask, inspectMappings bool) (*writerObservation, error) {
	pid := task.tid
	base := writerTaskPath(task)
	raw, err := s.read(filepath.Join(base, "stat"), 65536)
	if err != nil {
		if writerTaskGone(task) {
			return nil, nil
		}
		return nil, refused("stage0_writer_process_unknown")
	}
	birth, zombie, err := writerBirth(raw)
	if err != nil {
		return nil, err
	}
	fds, err := os.ReadDir(filepath.Join(base, "fd"))
	if err != nil {
		if writerTaskGone(task) {
			return nil, nil
		}
		return nil, refused("stage0_writer_descriptors_unreadable")
	}
	if zombie && len(fds) != 0 {
		return nil, refused("stage0_writer_zombie_descriptor")
	}
	var devices map[uint64]uint64
	for _, fd := range fds {
		number, e := strconv.Atoi(fd.Name())
		if e != nil || number < 0 {
			return nil, refused("stage0_writer_descriptor_unknown")
		}
		s.fds++
		if s.fds > maxWriterFDs {
			return nil, refused("stage0_writer_inventory_limit")
		}
		info, e := s.descriptor(filepath.Join(base, "fdinfo", fd.Name()))
		if e != nil {
			if writerTaskGone(task) {
				return nil, nil
			}
			// A descriptor can close between ReadDir and fdinfo without its
			// process exiting. Only an actual ENOENT for that descriptor is safe.
			if os.IsNotExist(e) {
				if _, check := os.Lstat(filepath.Join(base, "fd", fd.Name())); os.IsNotExist(check) {
					continue
				}
			}
			return nil, refused("stage0_writer_descriptors_unreadable")
		}
		if !s.inodes[info.inode] {
			continue
		}
		if devices == nil {
			raw, e = s.read(filepath.Join(base, "mountinfo"), 8<<20)
			if e != nil {
				if writerTaskGone(task) {
					return nil, nil
				}
				return nil, refused("stage0_writer_mount_inventory_unknown")
			}
			devices, e = mountDevices(raw)
			if e != nil {
				return nil, e
			}
		}
		device, known := devices[info.mount]
		if !known {
			return nil, refused("stage0_writer_device_unknown")
		}
		key := fileKey{device, info.inode}
		if s.keys[key] == nil {
			continue
		}
		access := info.flags & unix.O_ACCMODE
		if access != unix.O_RDONLY && access != unix.O_WRONLY && access != unix.O_RDWR {
			return nil, refused("stage0_writer_access_unknown")
		}
		if pid == os.Getpid() && s.witness[number] == key && access == unix.O_RDONLY {
			s.seenWitness[number] = true
		}
		if access == unix.O_WRONLY || access == unix.O_RDWR {
			s.add(key)
		}
	}
	if inspectMappings && !zombie {
		raw, err = s.read(filepath.Join(base, "maps"), 8<<20)
		if err != nil {
			if writerTaskGone(task) {
				return nil, nil
			}
			return nil, refused("stage0_writer_mappings_unreadable")
		}
		if err = s.inspectMappings(raw); err != nil {
			return nil, err
		}
	}
	raw, err = s.read(filepath.Join(base, "stat"), 65536)
	if err != nil {
		if writerTaskGone(task) {
			return nil, nil
		}
		return nil, refused("stage0_writer_process_unknown")
	}
	after, _, err := writerBirth(raw)
	if err != nil || after != birth {
		return nil, refused("stage0_writer_process_reused")
	}
	return &writerObservation{birth: birth, mappings: inspectMappings && !zombie}, nil
}

func (s *kernelScan) inspectMappings(raw []byte) error {
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		s.mappings++
		if s.mappings > maxWriterMappings {
			return refused("stage0_writer_inventory_limit")
		}
		parts := strings.Fields(line)
		if len(parts) < 5 || len(parts[1]) != 4 {
			return refused("stage0_writer_mapping_unknown")
		}
		inode, e := strconv.ParseUint(parts[4], 10, 64)
		if e != nil {
			return refused("stage0_writer_mapping_unknown")
		}
		if !s.inodes[inode] || parts[1][1] != 'w' || parts[1][3] != 's' {
			continue
		}
		pair := strings.Split(parts[3], ":")
		if len(pair) != 2 {
			return refused("stage0_writer_mapping_unknown")
		}
		major, e1 := strconv.ParseUint(pair[0], 16, 32)
		minor, e2 := strconv.ParseUint(pair[1], 16, 32)
		if e1 != nil || e2 != nil {
			return refused("stage0_writer_mapping_unknown")
		}
		s.add(fileKey{unix.Mkdev(uint32(major), uint32(minor)), inode})
	}
	return nil
}

func (s *kernelScan) listeners(port string) (int, error) {
	wanted, err := strconv.ParseUint(port, 10, 16)
	if err != nil || wanted == 0 {
		return 0, refused("stage0_health_target")
	}
	count := 0
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := s.read(path, 8<<20)
		if err != nil {
			return 0, refused("stage0_writer_listeners_unknown")
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) == 0 || !strings.Contains(lines[0], "local_address") {
			return 0, refused("stage0_writer_listeners_unknown")
		}
		for _, line := range lines[1:] {
			parts := strings.Fields(line)
			if len(parts) < 10 {
				return 0, refused("stage0_writer_listeners_unknown")
			}
			local := strings.Split(parts[1], ":")
			if len(local) != 2 {
				return 0, refused("stage0_writer_listeners_unknown")
			}
			p, err := strconv.ParseUint(local[1], 16, 16)
			if err != nil {
				return 0, refused("stage0_writer_listeners_unknown")
			}
			if p == wanted && parts[3] == "0A" {
				count++
			}
		}
	}
	return count, nil
}

func kernelWriters(targets []writerTarget, port string) ([]Writer, error) {
	s := kernelScan{keys: map[fileKey]map[string]bool{}, inodes: map[uint64]bool{}, witness: map[int]fileKey{}, seenWitness: map[int]bool{}, counts: map[string]int{}, deadline: time.Now().Add(30 * time.Second)}
	held := []heldWriterTarget{}
	metadataCount := 0
	defer func() {
		for _, target := range held {
			if target.fd >= 0 {
				unix.Close(target.fd)
			}
		}
	}()
	for _, target := range targets {
		fd, err := openWriterPath(unix.AT_FDCWD, target.path)
		if err != nil {
			if target.optional && os.IsNotExist(err) {
				held = append(held, heldWriterTarget{target: target, fd: -1, absent: true})
				continue
			}
			return nil, refused("stage0_writer_target_unavailable")
		}
		h := heldWriterTarget{target: target, fd: fd, nodes: map[string]writerStamp{}}
		held = append(held, h)
		stamp, err := writerStat(fd)
		if err != nil {
			return nil, err
		}
		held[len(held)-1].stamp = stamp
		if (stamp.mode&unix.S_IFMT == unix.S_IFDIR) != target.directory {
			return nil, refused("stage0_writer_target_changed")
		}
		if err = collectWriterNodes(fd, ".", h.nodes); err != nil {
			return nil, err
		}
		metadataCount += len(h.nodes)
		if metadataCount > maxWriterNodes {
			return nil, refused("stage0_writer_inventory_limit")
		}
		for _, node := range h.nodes {
			if s.keys[node.key] == nil {
				s.keys[node.key] = map[string]bool{}
			}
			s.keys[node.key][target.id] = true
			s.inodes[node.key.inode] = true
		}
		if len(s.keys) > maxWriterNodes {
			return nil, refused("stage0_writer_inventory_limit")
		}
		s.witness[fd] = stamp.key
	}
	seen := map[writerTask]*writerObservation{}
	mapped := map[int]bool{}
	for round := 0; round < 3; round++ {
		tasks, err := writerTasks()
		if err != nil {
			return nil, err
		}
		added := false
		for _, task := range tasks {
			if _, ok := seen[task]; ok {
				continue
			}
			observation, err := s.process(task, !mapped[task.tgid])
			if err != nil {
				return nil, err
			}
			seen[task] = observation
			if observation != nil && observation.mappings {
				mapped[task.tgid] = true
			}
			added = true
		}
		if !added {
			break
		}
		if round == 2 {
			return nil, refused("stage0_writer_process_inventory_unstable")
		}
	}
	for task, observation := range seen {
		if observation == nil {
			if !writerTaskGone(task) {
				return nil, refused("stage0_writer_process_reused")
			}
			continue
		}
		raw, err := s.read(filepath.Join(writerTaskPath(task), "stat"), 65536)
		if err != nil {
			if writerTaskGone(task) {
				continue
			}
			return nil, refused("stage0_writer_process_unknown")
		}
		after, _, err := writerBirth(raw)
		if err != nil || after != observation.birth {
			return nil, refused("stage0_writer_process_reused")
		}
	}
	if len(s.seenWitness) != len(s.witness) {
		return nil, refused("stage0_writer_visibility_unproven")
	}
	listeners, err := s.listeners(port)
	if err != nil {
		return nil, err
	}
	for _, h := range held {
		if err := recheckWriterTarget(h); err != nil {
			return nil, err
		}
	}
	result := []Writer{}
	if listeners > 0 {
		result = append(result, Writer{Kind: "listener", Count: listeners})
	}
	ids := []string{}
	for id := range s.counts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		result = append(result, Writer{Kind: "holder", Object: id, Count: s.counts[id]})
	}
	return result, nil
}

func recheckWriterTarget(h heldWriterTarget) error {
	current, err := openWriterPath(unix.AT_FDCWD, h.target.path)
	if h.absent {
		if err == nil {
			unix.Close(current)
			return refused("stage0_writer_target_changed")
		}
		if !os.IsNotExist(err) {
			return refused("stage0_writer_target_unknown")
		}
		return nil
	}
	if err != nil {
		return refused("stage0_writer_target_changed")
	}
	stamp, e := writerStat(current)
	unix.Close(current)
	if e != nil || stamp != h.stamp {
		return refused("stage0_writer_target_changed")
	}
	fresh := map[string]writerStamp{}
	if err = collectWriterNodes(h.fd, ".", fresh); err != nil {
		return err
	}
	if len(fresh) != len(h.nodes) {
		return refused("stage0_writer_target_changed")
	}
	for path, node := range h.nodes {
		if fresh[path] != node {
			return refused("stage0_writer_target_changed")
		}
	}

	return nil
}
