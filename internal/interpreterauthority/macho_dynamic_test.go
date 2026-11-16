package interpreterauthority

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func machoStringCommand(kind uint32, header int, name string) []byte {
	size := (header + len(name) + 1 + 7) &^ 7
	raw := make([]byte, size)
	binary.LittleEndian.PutUint32(raw, kind)
	binary.LittleEndian.PutUint32(raw[4:], uint32(size))
	binary.LittleEndian.PutUint32(raw[8:], uint32(header))
	copy(raw[header:], name)
	return raw
}

func machoFixture(commands ...[]byte) []byte {
	raw := make([]byte, 32)
	binary.LittleEndian.PutUint32(raw, 0xfeedfacf)
	binary.LittleEndian.PutUint32(raw[4:], 0x0100000c)
	binary.LittleEndian.PutUint32(raw[12:], 2)
	binary.LittleEndian.PutUint32(raw[16:], uint32(len(commands)))
	for _, command := range commands {
		raw = append(raw, command...)
	}
	binary.LittleEndian.PutUint32(raw[20:], uint32(len(raw)-32))
	return raw
}

func retainedMachODirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "routeb-macho-loader-test-")
	if err != nil {
		t.Fatal(err)
	}
	// Retain generated native-image fixtures for inspection by the test runner.
	t.Logf("retained native-image fixtures: %s", directory)
	return directory
}

func writeMachO(t *testing.T, directory, name string, raw []byte) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMachOEnumeratesAllLoaderLibraryKinds(t *testing.T) {
	directory := retainedMachODirectory(t)
	names := []string{"/libraries/regular.dylib", "/libraries/weak.dylib", "/libraries/reexport.dylib", "/libraries/lazy.dylib", "/libraries/upward.dylib"}
	kinds := []uint32{0xc, 0x80000018, 0x8000001f, 0x20, 0x80000023}
	commands := [][]byte{machoStringCommand(0xd, 24, "/libraries/self.dylib")}
	for index, kind := range kinds {
		commands = append(commands, machoStringCommand(kind, 24, names[index]))
	}
	commands = append(commands, machoStringCommand(0x8000001c, 12, "/libraries"))
	name := writeMachO(t, directory, "image", machoFixture(commands...))
	actual, native, err := inspectNative(name, "arm64")
	if err != nil || !native || actual.format != "macho" || !slices.Equal(actual.needed, names) || !slices.Equal(actual.rpath, []string{"/libraries"}) {
		t.Fatalf("incomplete Mach-O loader view: native=%v image=%#v error=%v", native, actual, err)
	}
}

func TestMachORejectsMalformedLoaderStrings(t *testing.T) {
	ordinary := func() []byte { return machoStringCommand(0xc, 24, "/libraries/library.dylib") }
	weak := func() []byte { return machoStringCommand(0x80000018, 24, "/libraries/library.dylib") }
	rpath := func() []byte { return machoStringCommand(0x8000001c, 12, "/libraries") }
	cases := map[string]func() []byte{
		"ordinary-name-in-header":    func() []byte { b := ordinary(); binary.LittleEndian.PutUint32(b[8:], 8); return b },
		"weak-name-outside-command":  func() []byte { b := weak(); binary.LittleEndian.PutUint32(b[8:], uint32(len(b))); return b },
		"weak-command-too-short":     func() []byte { b := weak()[:16]; binary.LittleEndian.PutUint32(b[4:], 16); return b },
		"empty-weak-name":            func() []byte { return machoStringCommand(0x80000018, 24, "") },
		"unterminated-ordinary-name": func() []byte { b := ordinary(); copy(b[24:], bytes.Repeat([]byte{'x'}, len(b)-24)); return b },
		"invalid-utf8-weak-name":     func() []byte { b := weak(); b[24] = 0xff; return b },
		"rpath-name-in-header":       func() []byte { b := rpath(); binary.LittleEndian.PutUint32(b[8:], 8); return b },
		"empty-rpath":                func() []byte { return machoStringCommand(0x8000001c, 12, "") },
		"unaligned-command": func() []byte {
			b := ordinary()
			b = b[:len(b)-1]
			binary.LittleEndian.PutUint32(b[4:], uint32(len(b)))
			return b
		},
	}
	directory := retainedMachODirectory(t)
	for label, command := range cases {
		t.Run(label, func(t *testing.T) {
			name := writeMachO(t, directory, label, machoFixture(command()))
			if _, _, err := inspectNative(name, "arm64"); err == nil {
				t.Fatal("accepted malformed loader string or command")
			}
		})
	}
}

func TestMachORejectsUnmodeledLoaderCommands(t *testing.T) {
	directory := retainedMachODirectory(t)
	for label, kind := range map[string]uint32{"load-fvmlib": 0x6, "fvmfile": 0x9, "prebound-dylib": 0x10, "dyld-environment": 0x27, "unknown-optional": 0x7e, "chained-fixups": 0x80000034, "exports-trie": 0x80000033} {
		t.Run(label, func(t *testing.T) {
			name := writeMachO(t, directory, label, machoFixture(machoStringCommand(kind, 24, "DYLD_LIBRARY_PATH=/unbound")))
			if _, _, err := inspectNative(name, "arm64"); err == nil {
				t.Fatal("accepted unmodeled loader behavior")
			}
		})
	}
}

func TestMachOWeakLibraryCannotBeOmittedOrLeftUnbound(t *testing.T) {
	directory := retainedMachODirectory(t)
	dependency := filepath.Join(directory, "weak.dylib")
	executable := writeMachO(t, directory, "python", machoFixture(machoStringCommand(0x80000018, 24, dependency)))
	document := Document{
		Binding:      Binding{Platform: "darwin", Architecture: "arm64"},
		Prefix:       Tree{Root: directory, Entries: []Entry{{Path: "python", Kind: "file"}}},
		Executable:   "python",
		NativeImages: []NativeImage{{Path: executable, Format: "macho", Needed: []string{}, RPath: []string{}, RunPath: []string{}, Resolution: map[string]string{}}},
	}
	if err := verifyNativeImages(document); err == nil || !strings.Contains(err.Error(), "authority_native_image_changed") {
		t.Fatalf("unlisted weak load escaped exact inventory: %v", err)
	}
	document.NativeImages[0].Needed = []string{dependency}
	document.NativeImages[0].Resolution[dependency] = dependency
	if err := verifyNativeImages(document); err == nil || !strings.Contains(err.Error(), "authority_native_dependency_missing") {
		t.Fatalf("weak load accepted without a bound native object: %v", err)
	}
	writeMachO(t, directory, "weak.dylib", machoFixture())
	document.Prefix.Entries = append(document.Prefix.Entries, Entry{Path: "weak.dylib", Kind: "file"})
	document.NativeImages = append(document.NativeImages, NativeImage{Path: dependency, Format: "macho", Needed: []string{}, RPath: []string{}, RunPath: []string{}, Resolution: map[string]string{}})
	if err := verifyNativeImages(document); err != nil {
		t.Fatalf("complete bound dependency refused: %v", err)
	}
}

func TestMachOLoaderByteOrdersAndRawCommandSize(t *testing.T) {
	directory := retainedMachODirectory(t)
	for label, order := range map[string]binary.ByteOrder{"little": binary.LittleEndian, "big": binary.BigEndian} {
		t.Run(label, func(t *testing.T) {
			command := machoStringCommand(0x80000018, 24, "/libraries/weak.dylib")
			for offset := 0; offset < 24; offset += 4 {
				order.PutUint32(command[offset:], binary.LittleEndian.Uint32(command[offset:]))
			}
			raw := machoFixture(command)
			for offset := 0; offset < 32; offset += 4 {
				order.PutUint32(raw[offset:], binary.LittleEndian.Uint32(raw[offset:]))
			}
			name := writeMachO(t, directory, label, raw)
			actual, native, err := inspectNative(name, "arm64")
			if err != nil || !native || !slices.Equal(actual.needed, []string{"/libraries/weak.dylib"}) {
				t.Fatalf("lost %s-endian library command: native=%v image=%#v error=%v", label, native, actual, err)
			}
			for _, bad := range []string{"raw-size-mismatch", "name-in-header", "name-past-end", "missing-nul"} {
				t.Run(bad, func(t *testing.T) {
					mutated := append([]byte{}, command...)
					switch bad {
					case "raw-size-mismatch":
						order.PutUint32(mutated[4:], uint32(len(mutated)-8))
					case "name-in-header":
						order.PutUint32(mutated[8:], 8)
					case "name-past-end":
						order.PutUint32(mutated[8:], uint32(len(mutated)))
					case "missing-nul":
						copy(mutated[24:], bytes.Repeat([]byte{'x'}, len(mutated)-24))
					}
					image := &macho.File{
						FileHeader: macho.FileHeader{Magic: macho.Magic64},
						ByteOrder:  order,
						Loads:      []macho.Load{macho.LoadBytes(mutated)},
					}
					if _, _, err := machoDynamic(image); err == nil {
						t.Fatal("accepted malformed raw loader command")
					}
				})
			}
		})
	}
}

func fatContainer(magic []byte, wide, swapped bool) []byte {
	order := binary.ByteOrder(binary.BigEndian)
	if swapped {
		order = binary.LittleEndian
	}
	numeric := uint32(0xcafebabe)
	entrySize := 20
	if wide {
		numeric = 0xcafebabf
		entrySize = 32
	}
	raw := make([]byte, 128)
	order.PutUint32(raw[:4], numeric)
	order.PutUint32(raw[4:8], 2)
	for index, cpu := range []uint32{0x0100000c, 0x01000007} {
		entry := raw[8+index*entrySize:]
		offset := 128 + index*32
		order.PutUint32(entry, cpu)
		if wide {
			order.PutUint64(entry[8:], uint64(offset))
			order.PutUint64(entry[16:], 32)
			order.PutUint32(entry[24:], 3)
		} else {
			order.PutUint32(entry[8:], uint32(offset))
			order.PutUint32(entry[12:], 32)
			order.PutUint32(entry[16:], 3)
		}
		thin := machoFixture()
		binary.LittleEndian.PutUint32(thin[4:], cpu)
		raw = append(raw, thin...)
	}
	return raw
}

func hostileFatTail(magic []byte) []byte {
	raw := bytes.Repeat([]byte{0xff}, 32)
	copy(raw, magic)
	return raw
}

func requireFatRefusal(t *testing.T, raw []byte, architecture string) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "container")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	image, native, err := inspectNative(path, architecture)
	t.Logf("magic=%x architecture=%s native=%v inspect_error=%v", raw[:4], architecture, native, err)
	if !native || err == nil || err.Error() != "authority_fat_image_unsupported" {
		t.Errorf("unsupported fat signature escaped explicit refusal: native=%v error=%v image=%#v", native, err, image)
	}
}

func requireFatInventoryRefusal(t *testing.T, raw []byte, external bool) {
	t.Helper()
	directory := t.TempDir()
	executable := writeMachO(t, directory, "python", machoFixture())
	container := writeMachO(t, directory, "container", raw)
	document := Document{
		Binding:      Binding{Platform: "darwin", Architecture: "arm64"},
		Prefix:       Tree{Root: directory, Entries: []Entry{{Path: "python", Kind: "file"}}},
		Executable:   "python",
		NativeImages: []NativeImage{{Path: executable, Format: "macho", Needed: []string{}, RPath: []string{}, RunPath: []string{}, Resolution: map[string]string{}}},
	}
	if external {
		document.External = []External{{Entry: Entry{Path: container, Kind: "file"}}}
	} else {
		document.Prefix.Entries = append(document.Prefix.Entries, Entry{Path: "container", Kind: "file"})
	}
	err := verifyNativeImages(document)
	t.Logf("external=%v inventory_error=%v", external, err)
	if err == nil || err.Error() != "authority_fat_image_unsupported" {
		t.Errorf("native inventory accepted an omitted unsupported container: %v", err)
	}
}

func requireFatCatalogRefusal(t *testing.T, raw []byte) {
	t.Helper()
	fixture := newFixture(t, false)
	externalPath := filepath.Join(fixture.policy.boundary, "external-container")
	assetPath := filepath.Join(fixture.policy.boundary, "recovery-copy")
	for _, path := range []string{externalPath, assetPath} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixture.catalog.External = []External{{
		Entry: Entry{Path: externalPath, Kind: "file", Metadata: metadataFor(t, externalPath), File: &Content{SHA256: hashBytes(raw), Size: int64(len(raw))}},
		Asset: &File{Path: assetPath, SHA256: hashBytes(raw), Size: int64(len(raw))},
	}}
	catalogJSON, err := json.Marshal(fixture.catalog)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(catalogJSON)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := verifyFiles(parsed, fixture.policy)
	t.Logf("catalog_error=%v catalog_verified=%v native_execution_allowed=%v", err, evidence.CatalogVerified, evidence.NativeExecutionAllowed)
	if err == nil || err.Error() != "authority_fat_image_unsupported" || evidence.CatalogVerified {
		t.Errorf("complete offline catalog accepted an unsupported external container: error=%v verified=%v", err, evidence.CatalogVerified)
	}
	if evidence.NativeExecutionAllowed {
		t.Fatal("offline fixture must never authorize native execution")
	}
}

func TestFatMarkersRefuseWithoutSelectingHostSlice(t *testing.T) {
	if os.Getuid() == 0 {
		t.Fatal("fat refusal test must run without root")
	}
	for _, marker := range []struct {
		name    string
		magic   []byte
		wide    bool
		swapped bool
	}{
		{"FAT_MAGIC", []byte{0xca, 0xfe, 0xba, 0xbe}, false, false},
		{"FAT_CIGAM", []byte{0xbe, 0xba, 0xfe, 0xca}, false, true},
		{"FAT_MAGIC_64", []byte{0xca, 0xfe, 0xba, 0xbf}, true, false},
		{"FAT_CIGAM_64", []byte{0xbf, 0xba, 0xfe, 0xca}, true, true},
	} {
		t.Run(marker.name, func(t *testing.T) {
			container := fatContainer(marker.magic, marker.wide, marker.swapped)
			if !bytes.Equal(container[:4], marker.magic) {
				t.Fatalf("synthetic container magic = %x, want %x", container[:4], marker.magic)
			}
			for _, shape := range []struct {
				name string
				raw  []byte
			}{
				{"four_bytes", append([]byte{}, marker.magic...)},
				{"container", container},
				{"truncated", append(append([]byte{}, marker.magic...), 0x00, 0x00, 0x02)},
				{"hostile_tail", hostileFatTail(marker.magic)},
			} {
				t.Run(shape.name, func(t *testing.T) {
					for _, architecture := range []string{"arm64", "amd64"} {
						requireFatRefusal(t, shape.raw, architecture)
					}
				})
			}
			t.Run("prefix_inventory", func(t *testing.T) {
				requireFatInventoryRefusal(t, container, false)
			})
			t.Run("external_inventory", func(t *testing.T) {
				requireFatInventoryRefusal(t, container, true)
			})
			t.Run("offline_catalog", func(t *testing.T) {
				requireFatCatalogRefusal(t, container)
			})
		})
	}
}

func TestShortAndOrdinaryFilesRemainNonNative(t *testing.T) {
	directory := t.TempDir()
	for _, item := range []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"one_byte", []byte{0xca}},
		{"two_bytes", []byte{0xca, 0xfe}},
		{"three_bytes", []byte{0xca, 0xfe, 0xba}},
		{"ordinary_text", []byte("print('not native')\n")},
		{"unrelated_four_bytes", []byte{0x00, 0x01, 0x02, 0x03}},
	} {
		t.Run(item.name, func(t *testing.T) {
			path := filepath.Join(directory, item.name)
			if err := os.WriteFile(path, item.raw, 0600); err != nil {
				t.Fatal(err)
			}
			image, native, err := inspectNative(path, "arm64")
			if native || err != nil || image.format != "" {
				t.Fatalf("short or ordinary file became native: native=%v error=%v image=%#v", native, err, image)
			}
		})
	}
}

func TestReviewUnknownRequiredLoaderCommandMustRefuse(t *testing.T) {
	directory := retainedMachODirectory(t)
	for _, kind := range []uint32{0x8000007e, 0x80000035} {
		name := writeMachO(t, directory, "unknown", machoFixture(machoStringCommand(kind, 32, "/unbound/image")))
		actual, native, err := inspectNative(name, "arm64")
		if err == nil {
			t.Errorf("unmodeled required load command %#x accepted: native=%v observed=%#v", kind, native, actual)
		}
	}
}
