//go:build linux

package routebstage0

import (
	"bytes"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

type writerTarget struct {
	id, path  string
	directory bool
	optional  bool
}

func (s *supervisor) writerTargets() ([]writerTarget, string, error) {
	l := s.loaded
	if l.registration.Inputs.Package == nil {
		return nil, "", refused("stage0_package_required")
	}
	raw, err := readInput(*l.registration.Inputs.Package, l.host.UID)
	if err != nil {
		return nil, "", err
	}
	var pkg map[string]any
	if err = decodeExact(raw, &pkg); err != nil {
		return nil, "", err
	}
	readSide := func(name, key string) (map[string]any, error) {
		wanted, ok := pkg[key].(string)
		if !ok || !validDigest(wanted) {
			return nil, refused("stage0_package_binding")
		}
		path := filepath.Join(filepath.Dir(l.registration.Inputs.Package.Path), name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, refused("stage0_package_unavailable")
		}
		raw, err := readInput(interpreterauthority.File{Path: path, SHA256: wanted, Size: info.Size()}, l.host.UID)
		if err != nil {
			return nil, err
		}
		var result map[string]any
		if err = decodeExact(raw, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
	group, err := readSide("recovery-group.json", "recovery_group_sha256")
	if err != nil {
		return nil, "", err
	}
	profile, err := readSide("native-profile.json", "native_profile_sha256")
	if err != nil {
		return nil, "", err
	}
	if profile["schema"] != "ags.routeb-native-profile.v2" || profile["root"] != l.host.Root {
		return nil, "", refused("stage0_profile_binding")
	}
	objects, ok := group["objects"].([]any)
	if !ok || len(objects) == 0 || len(objects) > 256 {
		return nil, "", refused("stage0_group_shape")
	}
	targets := []writerTarget{}
	ids := map[string]bool{}
	for _, raw := range objects {
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, "", refused("stage0_group_shape")
		}
		id, ok1 := object["id"].(string)
		path, ok2 := object["source"].(string)
		if !ok1 || !ok2 || !interpreterauthority.ValidID(id) || ids[id] || !absolute(path) {
			return nil, "", refused("stage0_group_shape")
		}
		ids[id] = true
		allowed := within(path, l.host.Root) || within(path, l.host.ToolBundle) || path == l.registration.HostProfile.Path
		for _, i := range l.registration.Invocations {
			if i.Operation == "serve" && path == i.Definition.Path {
				allowed = true
			}
		}
		if !allowed {
			return nil, "", refused("stage0_writer_target_outside_registration")
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil, "", refused("stage0_writer_target_unknown")
		}
		targets = append(targets, writerTarget{id: id, path: path, directory: info.IsDir()})
		if object["role"] == "sqlite" {
			for _, suffix := range []string{"-wal", "-shm"} {
				targets = append(targets, writerTarget{id: id, path: path + suffix, optional: true})
			}
		}
	}
	capture, ok := profile["capture"].(map[string]any)
	if !ok {
		return nil, "", refused("stage0_profile_shape")
	}
	health, ok := capture["health"].(map[string]any)
	if !ok {
		return nil, "", refused("stage0_profile_shape")
	}
	endpoint, ok := health["local"].(string)
	if !ok {
		return nil, "", refused("stage0_profile_shape")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/readyz" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, "", refused("stage0_health_target")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
		return nil, "", refused("stage0_health_target")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return nil, "", refused("stage0_health_target")
	}
	return targets, strconv.Itoa(n), nil
}

type boundedOutput struct {
	bytes.Buffer
	maximum int
}

func (b *boundedOutput) Write(raw []byte) (int, error) {
	if b.Len()+len(raw) > b.maximum {
		return 0, refused("stage0_command_output_limit")
	}
	return b.Buffer.Write(raw)
}

func (s *supervisor) writerProbe(generation string) (WriterResult, error) {
	result := WriterResult{Generation: generation, Writers: []Writer{}, Unknown: []string{}}
	operation := s.loaded.invocation.Operation
	if !interpreterauthority.ValidID(generation) || (operation != "transaction" && operation != "restore" && operation != "old-start") {
		return result, refused("stage0_writer_phase_invalid")
	}
	stopped := ""
	if operation == "old-start" {
		completion, err := s.restoreResult()
		if err != nil {
			return result, err
		}
		stopped = completion.StoppedInvocationID
	} else {
		if s.stopped == nil {
			return result, refused("stage0_writer_phase_invalid")
		}
		stopped = s.stopped.Context.InvocationID
	}
	if _, err := s.runtimeExit(stopped); err != nil {
		return result, err
	}
	targets, port, err := s.writerTargets()
	if err != nil {
		return result, err
	}
	result.Writers, err = kernelWriters(targets, port)
	if err != nil {
		return result, err
	}
	result.Closed = len(result.Writers) == 0
	return result, nil
}
