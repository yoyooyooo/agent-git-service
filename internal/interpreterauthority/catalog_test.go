package interpreterauthority

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type fixture struct {
	catalog Document
	policy  fsPolicy
	root    string
}

func smallELF() []byte {
	if runtime.GOOS == "darwin" {
		data := make([]byte, 32)
		binary.LittleEndian.PutUint32(data, 0xfeedfacf)
		cpu := uint32(0x01000007)
		if runtime.GOARCH == "arm64" {
			cpu = 0x0100000c
		}
		binary.LittleEndian.PutUint32(data[4:], cpu)
		binary.LittleEndian.PutUint32(data[12:], 2)
		return data
	}
	data := make([]byte, 64)
	copy(data, "\x7fELF")
	data[4] = 2
	data[5] = 1
	data[6] = 1
	binary.LittleEndian.PutUint16(data[16:], 2)
	machine := uint16(62)
	if runtime.GOARCH == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(data[18:], machine)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	binary.LittleEndian.PutUint16(data[54:], 56)
	binary.LittleEndian.PutUint16(data[58:], 64)
	return data
}

func metadataFor(t *testing.T, name string) Metadata {
	t.Helper()
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	s := info.Sys().(*syscall.Stat_t)
	attrs, err := attributes(name)
	if err != nil {
		t.Fatal(err)
	}
	return Metadata{Mode: uint32(info.Mode().Perm()), GID: s.Gid, Device: uint64(s.Dev), Inode: uint64(s.Ino), MtimeNS: info.ModTime().UnixNano(), Xattrs: attrs}
}

func newFixture(t *testing.T, large bool) fixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "python")
	assets := filepath.Join(base, "assets")
	for _, name := range []string{root, filepath.Join(root, "bin"), filepath.Join(root, "lib"), filepath.Join(root, "lib/python3.13"), assets} {
		if err := os.MkdirAll(name, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, 0755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{"bin/python3.13": smallELF(), "lib/python3.13/os.py": []byte("value = 1\n")}
	if large {
		files["lib/libpython-large.a"] = bytes.Repeat([]byte{0x5a}, 17<<20)
	}
	for name, raw := range files {
		mode := os.FileMode(0644)
		if name == "bin/python3.13" {
			mode = 0755
		}
		if err := os.WriteFile(filepath.Join(root, name), raw, mode); err != nil {
			t.Fatal(err)
		}
		// Model the materializer's explicit publisher-mode normalization even
		// when a private native test runner deliberately uses umask 077.
		if err := os.Chmod(filepath.Join(root, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	link := "python3.13"
	linkPath := filepath.Join(root, "bin/python3")
	if err := os.Symlink(link, linkPath); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		if err := unix.Fchmodat(unix.AT_FDCWD, linkPath, 0777, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Fatal(err)
		}
	}
	entries := []Entry{}
	err := filepath.WalkDir(root, func(name string, item os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		entry := Entry{Path: filepath.ToSlash(rel), Metadata: metadataFor(t, name)}
		switch {
		case item.IsDir():
			entry.Kind = "directory"
		case item.Type()&os.ModeSymlink != 0:
			entry.Kind = "symlink"
			entry.Link = &link
		default:
			entry.Kind = "file"
			raw := files[entry.Path]
			entry.File = &Content{hashBytes(raw), int64(len(raw))}
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	writer := tar.NewWriter(gz)
	for _, entry := range entries {
		h := &tar.Header{Name: "python/" + entry.Path, Mode: int64(entry.Metadata.Mode), Uid: 0, Gid: 0, ModTime: time.Unix(0, 0)}
		if entry.Path == "." {
			h.Name = "python/"
		}
		switch entry.Kind {
		case "directory":
			h.Typeflag = tar.TypeDir
		case "symlink":
			h.Typeflag = tar.TypeSymlink
			h.Linkname = *entry.Link
		case "file":
			h.Typeflag = tar.TypeReg
			h.Size = entry.File.Size
		}
		if err := writer.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if entry.File != nil {
			if _, err := writer.Write(files[entry.Path]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	assetName := "cpython-3.13.18+20261026-x86_64-unknown-linux-gnu-install_only_stripped.tar.gz"
	archivePath := filepath.Join(assets, assetName)
	if err := os.WriteFile(archivePath, archive.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	checksums := []byte(hashBytes(archive.Bytes()) + "  " + assetName + "\n")
	checksumsPath := filepath.Join(assets, "SHA256SUMS")
	if err := os.WriteFile(checksumsPath, checksums, 0644); err != nil {
		t.Fatal(err)
	}
	format := "elf"
	if runtime.GOOS == "darwin" {
		format = "macho"
	}
	value := Document{Schema: Schema, ID: "fixture", Binding: Binding{HostInstance: "fixture-host", HostProfileSHA256: strings.Repeat("a", 64), ToolRevision: strings.Repeat("b", 40), ToolTree: strings.Repeat("c", 40), ToolManifestSHA256: strings.Repeat("d", 64), Platform: runtime.GOOS, Architecture: runtime.GOARCH},
		Publisher: Publisher{Repository: "astral-sh/python-build-standalone", Release: "20261026", AssetName: assetName, Archive: File{archivePath, hashBytes(archive.Bytes()), int64(archive.Len())}, Checksums: File{checksumsPath, hashBytes(checksums), int64(len(checksums))}},
		Prefix:    Tree{root, entries}, Executable: "bin/python3.13", External: []External{}, SearchPaths: []SearchPath{}, NativeImages: []NativeImage{{Path: filepath.Join(root, "bin/python3.13"), Format: format, Needed: []string{}, RPath: []string{}, RunPath: []string{}, Resolution: map[string]string{}}}, Environment: map[string]string{"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8"}}
	return fixture{value, fsPolicy{uint32(os.Getuid()), base}, root}
}

func TestCatalogRealFilesAndLargePublisherMember(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "above_ordinary_authority_limit"}[large], func(t *testing.T) {
			fixture := newFixture(t, large)
			raw, err := json.Marshal(fixture.catalog)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := verifyFiles(parsed, fixture.policy)
			if err != nil {
				t.Fatal(err)
			}
			if !evidence.CatalogVerified || evidence.NativeExecutionAllowed || evidence.PrefixMembers != len(parsed.Prefix.Entries) {
				t.Fatalf("invalid scoped evidence: %#v", evidence)
			}
		})
	}
}

func TestStrictJSONRejectsCaseAliasesDuplicatesMissingAndUnknown(t *testing.T) {
	value := newFixture(t, false).catalog
	raw, _ := json.Marshal(value)
	cases := map[string][]byte{
		"duplicate":         append([]byte(`{"schema":"wrong",`), raw[1:]...),
		"case_alias_before": append([]byte(`{"Schema":"wrong",`), raw[1:]...),
		"case_alias_after":  append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"Schema":"wrong"}`)...),
		"case_only":         bytes.Replace(raw, []byte(`"schema":`), []byte(`"Schema":`), 1),
		"unknown":           append([]byte(`{"ready":true,`), raw[1:]...),
		"trailing":          append(append([]byte{}, raw...), []byte(` {}`)...),
		"missing_zero_mode": bytes.Replace(raw, []byte(`"mode":493,`), nil, 1),
		"nested_case":       bytes.Replace(raw, []byte(`"mode":493`), []byte(`"mode":493,"Mode":511`), 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(data); err == nil {
				t.Fatal("accepted malformed authority")
			}
		})
	}
}

func TestCatalogRefusesFilesystemDrift(t *testing.T) {
	tests := map[string]func(*testing.T, *fixture){
		"stdlib_bytes": func(t *testing.T, f *fixture) {
			name := filepath.Join(f.root, "lib/python3.13/os.py")
			if err := os.WriteFile(name, []byte("value = 2\n"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"extra_member": func(t *testing.T, f *fixture) {
			if err := os.WriteFile(filepath.Join(f.root, "lib/injected.py"), []byte("injected"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"writable_directory": func(t *testing.T, f *fixture) {
			if err := os.Chmod(filepath.Join(f.root, "lib"), 0775); err != nil {
				t.Fatal(err)
			}
		},
		"writable_ancestor": func(t *testing.T, f *fixture) {
			if err := os.Chmod(f.policy.boundary, 0777); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(t *testing.T, f *fixture) {
			if err := os.Link(filepath.Join(f.root, "bin/python3.13"), filepath.Join(f.policy.boundary, "hardlink")); err != nil {
				t.Fatal(err)
			}
		},
		"unexpected_xattr": func(t *testing.T, f *fixture) {
			key := "user.routeb-test"
			if runtime.GOOS == "darwin" {
				key = "com.example.routeb-test"
			}
			if err := unix.Lsetxattr(filepath.Join(f.root, "lib/python3.13/os.py"), key, []byte("changed"), 0); err != nil {
				t.Fatal(err)
			}
		},
		"changed_archive": func(t *testing.T, f *fixture) {
			if err := os.WriteFile(f.catalog.Publisher.Archive.Path, []byte("changed"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"forged_subset": func(t *testing.T, f *fixture) {
			for i, e := range f.catalog.Prefix.Entries {
				if e.Path == "lib/python3.13/os.py" {
					f.catalog.Prefix.Entries = append(f.catalog.Prefix.Entries[:i], f.catalog.Prefix.Entries[i+1:]...)
					break
				}
			}
		},
		"missing_image": func(t *testing.T, f *fixture) { f.catalog.NativeImages = []NativeImage{} },
		"changed_native_header": func(t *testing.T, f *fixture) {
			for i := range f.catalog.NativeImages {
				f.catalog.NativeImages[i].Interpreter = "/not/registered/loader"
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, false)
			mutate(t, &f)
			if _, err := verifyFiles(f.catalog, f.policy); err == nil {
				t.Fatal("accepted drift")
			}
		})
	}
}

func TestLinkGraphRefusesEscapeCycleAndMissingTarget(t *testing.T) {
	for _, target := range []string{"../../outside", "/outside", "missing", "python3"} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t, false)
			for i := range f.catalog.Prefix.Entries {
				if f.catalog.Prefix.Entries[i].Kind == "symlink" {
					f.catalog.Prefix.Entries[i].Link = &target
				}
			}
			if err := f.catalog.Validate(); err == nil {
				t.Fatal("accepted invalid link")
			}
		})
	}
}

func TestArchiveRejectsAmbiguousAndUnsafeMembers(t *testing.T) {
	tests := map[string][]tar.Header{
		"escape":         {{Name: "python/../../escape", Mode: 0644, Typeflag: tar.TypeReg}},
		"hardlink":       {{Name: "python/hard", Mode: 0644, Typeflag: tar.TypeLink, Linkname: "python/other"}},
		"device":         {{Name: "python/device", Mode: 0600, Typeflag: tar.TypeChar}},
		"suid":           {{Name: "python/bin/python", Mode: 04755, Typeflag: tar.TypeReg}},
		"duplicate":      {{Name: "python/file", Mode: 0644, Typeflag: tar.TypeReg}, {Name: "python/file", Mode: 0644, Typeflag: tar.TypeReg}},
		"symlink_parent": {{Name: "python/lib", Mode: 0777, Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}, {Name: "python/lib/member", Mode: 0644, Typeflag: tar.TypeReg}},
	}
	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			var buffer bytes.Buffer
			gz := gzip.NewWriter(&buffer)
			writer := tar.NewWriter(gz)
			for i := range headers {
				if err := writer.WriteHeader(&headers[i]); err != nil {
					t.Fatal(err)
				}
			}
			writer.Close()
			gz.Close()
			if _, err := archiveInventory(bytes.NewReader(buffer.Bytes())); err == nil {
				t.Fatal("accepted unsafe tar")
			}
		})
	}
}

func TestChecksumRequiresOneExactPublisherAsset(t *testing.T) {
	p := Publisher{AssetName: "python.tar.gz", Archive: File{SHA256: strings.Repeat("a", 64)}}
	good := []byte(p.Archive.SHA256 + "  " + p.AssetName + "\n")
	if err := checksumMatches(good, p); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{nil, append(append([]byte{}, good...), good...), []byte(strings.Repeat("b", 64) + "  " + p.AssetName + "\n")} {
		if err := checksumMatches(raw, p); err == nil {
			t.Fatal("accepted unbound checksum")
		}
	}
}

func TestSearchPathAbsenceAndMembership(t *testing.T) {
	f := newFixture(t, false)
	absent := SearchPath{Path: filepath.Join(f.policy.boundary, "absent/subdirectory"), State: "absent", Members: []string{}}
	if err := verifySearch(absent, f.policy); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(absent.Path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := verifySearch(absent, f.policy); err == nil {
		t.Fatal("accepted appeared search path")
	}
	search := SearchPath{Path: filepath.Join(f.root, "bin"), State: "closed-directory", Members: []string{"python3", "python3.13"}}
	sort.Strings(search.Members)
	if err := verifySearch(search, f.policy); err != nil {
		t.Fatal(err)
	}
	search.Members = search.Members[:1]
	if err := verifySearch(search, f.policy); err == nil {
		t.Fatal("accepted incomplete search membership")
	}
}

func TestRegisteredVerifierDoesNotAcceptCallerPathOrFixtureOwnership(t *testing.T) {
	f := newFixture(t, false)
	for _, id := range []string{f.root, "../fixture", "fixture/authority.json", "..", "."} {
		if _, err := VerifyRegistered(id, strings.Repeat("a", 64), f.catalog.Binding); err == nil {
			t.Fatal("accepted caller registry path")
		}
	}
	if os.Getuid() != 0 {
		if _, _, err := openTrusted(f.catalog.Publisher.Archive.Path, fsPolicy{0, "/"}, MaxArchive); err == nil {
			t.Fatal("accepted user-owned authority")
		}
	}
}
