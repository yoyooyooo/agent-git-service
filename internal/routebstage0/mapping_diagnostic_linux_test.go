//go:build linux

package routebstage0

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
	"golang.org/x/sys/unix"
)

func mappingFixture() (map[string]interpreterauthority.FileIdentity, string, string) {
	main := "/opt/fixture/python/bin/python3.13"
	known := map[string]interpreterauthority.FileIdentity{
		main:                     {Path: main, Device: unix.Mkdev(8, 1), Inode: 41},
		"/usr/lib/example.cache": {Path: "/usr/lib/example.cache", Device: unix.Mkdev(8, 1), Inode: 42},
	}
	return known, main, "1000-2000 r-xp 00000000 08:01 41 " + main + "\n"
}

func TestMappingRefusalLocatesEachExistingPredicate(t *testing.T) {
	known, main, prefix := mappingFixture()
	for _, tc := range []struct {
		name, row, code, branch, match string
		expected                       bool
	}{
		{"zero inode named", "2000-3000 rw-p 00000000 00:00 0 /memfd:example", "stage0_mapping_unknown", "zero_inode_named", "not_checked", false},
		{"deleted row", "2000-3000 r--p 00000000 08:01 42 /usr/lib/example.cache (deleted)", "stage0_mapping_unknown", "file_path_shape", "not_checked", false},
		{"space in path", "2000-3000 r--p 00000000 08:01 42 /opt/path with spaces/cache", "stage0_mapping_unknown", "file_path_shape", "not_checked", false},
		{"relative path", "2000-3000 r--p 00000000 08:01 42 example.cache", "stage0_mapping_unknown", "file_path_shape", "not_checked", false},
		{"unregistered", "2000-3000 r--p 00000000 08:01 43 /usr/lib/not-registered.cache", "stage0_mapping_unknown", "catalog_file_identity", "absent", false},
		{"inode changed", "2000-3000 r--p 00000000 08:01 99 /usr/lib/example.cache", "stage0_mapping_unknown", "catalog_file_identity", "present", true},
		{"device changed", "2000-3000 r--p 00000000 08:02 42 /usr/lib/example.cache", "stage0_mapping_changed", "device_identity", "present", true},
		{"device invalid", "2000-3000 r--p 00000000 invalid 42 /usr/lib/example.cache", "stage0_mapping_changed", "device_identity", "present", true},
		{"missing fields", "2000-3000 r--p", "stage0_mapping_invalid", "field_count", "not_checked", false},
		{"inode invalid", "2000-3000 r--p 00000000 08:01 invalid /usr/lib/example.cache", "stage0_mapping_invalid", "inode_parse", "not_checked", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(prefix + tc.row + "\n")
			err := verifyMappingRows(raw, known, main)
			if errorCode(err) != tc.code || err.Error() != tc.code {
				t.Fatalf("primary refusal changed: %v", err)
			}
			var failure *mappingRefusal
			if !errors.As(err, &failure) {
				t.Fatalf("refusal lost its actual mapping object: %T", err)
			}
			d := failure.detail
			if d.Branch != tc.branch || d.Line != tc.row || d.LineNumber != 2 || d.FieldCount != len(strings.Fields(tc.row)) || d.CatalogMatch != tc.match ||
				failure.mapsSHA256 != hash(raw) || failure.mapsBytes != len(raw) {
				t.Fatalf("wrong diagnostic: %#v", failure)
			}
			if tc.expected {
				if d.Expected == nil || d.Expected.Inode != 42 || d.Expected.Device != unix.Mkdev(8, 1) {
					t.Fatalf("registered identity lost: %#v", d.Expected)
				}
			} else if d.Expected != nil {
				t.Fatal("invented a catalog identity for an unchecked or absent path")
			}
		})
	}
}

func TestMappingRefusalLeavesSuccessAndMissingMainUnchanged(t *testing.T) {
	known, main, prefix := mappingFixture()
	before := make(map[string]interpreterauthority.FileIdentity, len(known))
	for key, value := range known {
		before[key] = value
	}
	rows := prefix + "2000-3000 r--p 00000000 08:01 42 /usr/lib/example.cache\n" +
		"3000-4000 rw-p 00000000 00:00 0\n4000-5000 r--p 00000000 00:00 0 [vvar_vclock]\n"
	if err := verifyMappingRows([]byte(rows), known, main); err != nil {
		t.Fatal(err)
	}
	err := verifyMappingRows([]byte("3000-4000 rw-p 00000000 00:00 0\n"), known, main)
	if errorCode(err) != "stage0_python_main_not_mapped" {
		t.Fatalf("missing-main policy changed: %v", err)
	}
	_ = verifyMappingRows([]byte(prefix+"2000-3000 r--p 00000000 08:01 43 /usr/lib/new.cache\n"), known, main)
	if !reflect.DeepEqual(known, before) {
		t.Fatal("observing a mapping mutated the catalog")
	}
}

func TestMappingRefusalRecordBindsCheckedSubjectAndStaysExclusive(t *testing.T) {
	known, main, prefix := mappingFixture()
	err := verifyMappingRows([]byte(prefix+"2000-3000 r--p 00000000 08:01 99 /usr/lib/example.cache\n"), known, main)
	l := &loaded{registrationSHA: strings.Repeat("a", 64), invocation: Invocation{ID: "controller-one"},
		registration: Registration{Interpreter: Reference{SHA256: strings.Repeat("b", 64)}}}
	s := &supervisor{loaded: l}
	subject := Context{InvocationID: "serve-initial-one", Phase: "serve-initial", BootID: "fixture-boot",
		Supervisor: ProcessIdentity{PID: 500, StartTicks: 100, ParentPID: 1, ExecutableSHA256: strings.Repeat("c", 64)},
		Child:      ProcessIdentity{PID: 501, StartTicks: 101, ParentPID: 500, UID: 1001, GID: 1001, ExecutableSHA256: strings.Repeat("d", 64)}}
	record := s.mappingFailure(err, subject)
	if record == nil || record.RegistrationSHA256 != l.registrationSHA || record.CatalogSHA256 != l.registration.Interpreter.SHA256 ||
		record.InvocationID != "controller-one" || record.SubjectInvocationID != subject.InvocationID || record.Phase != subject.Phase ||
		record.BootID != subject.BootID || record.Child != subject.Child || record.Supervisor != subject.Supervisor || record.Code != errorCode(err) {
		t.Fatalf("mapping bound to the wrong process or catalog: %#v", record)
	}
	if s.mappingFailure(refused("stage0_child_changed"), subject) != nil || s.mappingFailure(nil, subject) != nil {
		t.Fatal("unrelated failure was converted to mapping evidence")
	}
	dir, e := os.MkdirTemp("", "stage0-mapping-diagnostic-")
	if e != nil {
		t.Fatal(e)
	}
	t.Log("retained", dir)
	path := filepath.Join(dir, "failure.json")
	if e = writeExclusiveFile(path, record); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var actual mappingFailure
	if e = json.Unmarshal(raw, &actual); e != nil || !reflect.DeepEqual(&actual, record) {
		t.Fatal("persisted diagnostic differs", e)
	}
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("diagnostic is not private", e)
	}
	if writeExclusiveFile(path, map[string]string{"replacement": "forbidden"}) == nil {
		t.Fatal("diagnostic was overwritten")
	}
	fresh, e := os.ReadFile(path)
	if e != nil || string(fresh) != string(raw) {
		t.Fatal("original refusal evidence changed", e)
	}
}
