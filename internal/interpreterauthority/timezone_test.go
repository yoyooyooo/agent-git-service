package interpreterauthority

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Exercise the declaration boundary without claiming a native ELF execution.
// The existing file verifier separately checks actual directory identities.
func timezoneCatalog(t *testing.T) Document {
	t.Helper()
	value := newFixture(t, false).catalog
	value.Binding.Platform = "linux"
	// Keep this declaration independent of the source-test host's metadata.
	for i := range value.Prefix.Entries {
		value.Prefix.Entries[i].Metadata.Xattrs = map[string]string{}
	}
	for i := range value.NativeImages {
		value.NativeImages[i].Format = "elf"
	}
	value.Linux = &LinuxEntry{
		Loader:           filepath.Join(value.Prefix.Root, value.Executable),
		LibraryDirectory: filepath.Join(Registry, value.ID, "runtime-libraries"),
	}
	directory := filepath.Join(Registry, value.ID, "timezone")
	value.Environment = map[string]string{"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TZ": "UTC0", "TZDIR": directory}
	value.External = []External{{Entry: Entry{Path: directory, Kind: "directory", Metadata: Metadata{
		Mode: 0755, Device: 1, Inode: 2, Xattrs: map[string]string{},
	}}}}
	value.SearchPaths = []SearchPath{{Path: directory, State: "closed-directory", Members: []string{}}}
	return value
}

func TestLinuxTimezoneRequiresBoundEnvironmentAndClosedDirectory(t *testing.T) {
	t.Run("fixed_utc_with_closed_lookup", func(t *testing.T) {
		if err := timezoneCatalog(t).Validate(); err != nil {
			t.Fatalf("rejected fixed UTC catalog: %v", err)
		}
	})
	tests := map[string]func(*Document){
		"host_default": func(value *Document) {
			delete(value.Environment, "TZ")
			delete(value.Environment, "TZDIR")
		},
		"utc_without_directory":      func(value *Document) { delete(value.Environment, "TZDIR") },
		"empty_timezone":             func(value *Document) { value.Environment["TZ"] = "" },
		"host_timezone":              func(value *Document) { value.Environment["TZ"] = ":/etc/localtime" },
		"host_lookup_directory":      func(value *Document) { value.Environment["TZDIR"] = "/usr/share/zoneinfo" },
		"extra_environment":          func(value *Document) { value.Environment["PYTHONPATH"] = "/untrusted" },
		"missing_directory_identity": func(value *Document) { value.External = nil },
		"directory_alias": func(value *Document) {
			entry := &value.External[0].Entry
			target := "elsewhere"
			entry.Kind, entry.Link, entry.Metadata.Mode = "symlink", &target, 0777
		},
		"missing_closed_lookup":              func(value *Document) { value.SearchPaths = nil },
		"absent_instead_of_closed_directory": func(value *Document) { value.SearchPaths[0].State = "absent" },
		"lookup_has_timezone_file":           func(value *Document) { value.SearchPaths[0].Members = []string{"UTC0"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := timezoneCatalog(t)
			mutate(&value)
			want := "authority_environment_invalid"
			switch name {
			case "missing_directory_identity", "directory_alias", "missing_closed_lookup", "absent_instead_of_closed_directory", "lookup_has_timezone_file":
				want = "authority_linux_search_unbound"
			}
			if err := value.Validate(); err == nil || err.Error() != want {
				t.Fatalf("timezone refusal = %v, want %s", err, want)
			}
		})
	}
}

func TestLinuxTimezoneEmptyDirectoryRejectsActualDrift(t *testing.T) {
	tests := map[string]func(string) error{
		"added_resource": func(directory string) error {
			return os.WriteFile(filepath.Join(directory, "UTC0"), []byte("replacement timezone"), 0644)
		},
		"writable_directory": func(directory string) error { return os.Chmod(directory, 0777) },
		"replacement_directory": func(directory string) error {
			if err := os.Rename(directory, directory+"-retained"); err != nil {
				return err
			}
			return os.Mkdir(directory, 0755)
		},
		"replacement_symlink": func(directory string) error {
			if err := os.Rename(directory, directory+"-retained"); err != nil {
				return err
			}
			return os.Symlink(directory+"-retained", directory)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t, false)
			directory := filepath.Join(fixture.policy.boundary, "timezone")
			if err := os.Mkdir(directory, 0755); err != nil {
				t.Fatal(err)
			}
			entry := Entry{Path: directory, Kind: "directory", Metadata: metadataFor(t, directory)}
			search := SearchPath{Path: directory, State: "closed-directory", Members: []string{}}
			check := func() error {
				if _, err := verifyEntry(directory, entry, fixture.policy); err != nil {
					return err
				}
				return verifySearch(search, fixture.policy)
			}
			if err := check(); err != nil {
				t.Fatal(err)
			}
			if err := mutate(directory); err != nil {
				t.Fatal(err)
			}
			if name == "added_resource" {
				if err := verifySearch(search, fixture.policy); err == nil || err.Error() != "authority_search_members_changed" {
					t.Fatalf("closed member check = %v", err)
				}
			}
			if err := check(); err == nil {
				t.Fatal("accepted changed timezone lookup")
			}
		})
	}
}

// A file-only catalog does not authorize a Linux entry. Preserve its existing
// locale contract and the Darwin declaration contract.
func TestCatalogEnvironmentWithoutLinuxEntryUnchanged(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			value := timezoneCatalog(t)
			value.Binding.Platform = platform
			value.Linux = nil
			value.Environment = map[string]string{"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8"}
			if err := value.Validate(); err != nil {
				t.Fatalf("locale-only catalog changed: %v", err)
			}
			value.Environment["TZ"] = "UTC0"
			if err := value.Validate(); err == nil || err.Error() != "authority_environment_invalid" {
				t.Fatalf("accepted an extra file-only environment variable: %v", err)
			}
		})
	}
}

func TestLinuxTimezoneProcessEnvironmentIsBoundCopy(t *testing.T) {
	t.Setenv("TZ", ":/etc/localtime")
	t.Setenv("TZDIR", "/unregistered")
	value := timezoneCatalog(t)
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	registered := &RegisteredCatalog{document: value}
	want := []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC0", "TZDIR=" + filepath.Join(Registry, value.ID, "timezone")}
	got := registered.ProcessEnvironment()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("process environment = %q, want %q", got, want)
	}
	got[0] = "PYTHONPATH=/unregistered"
	if !reflect.DeepEqual(registered.ProcessEnvironment(), want) {
		t.Fatal("caller changed the registered process environment")
	}
}
