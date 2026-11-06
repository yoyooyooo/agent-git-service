package interpreterauthority

import (
	"archive/tar"
	"bytes"
	"debug/elf"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

// The string table straddles two adjacent LOADs, matching the relevant shape
// of the published Python executable. No section headers are present.
func loaderFixture() []byte {
	b := make([]byte, 1024)
	copy(b, "\x7fELF")
	b[4] = 2
	b[5] = 1
	b[6] = 1
	binary.LittleEndian.PutUint16(b[16:], 2)
	binary.LittleEndian.PutUint16(b[18:], 62)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint64(b[32:], 64)
	binary.LittleEndian.PutUint16(b[52:], 64)
	binary.LittleEndian.PutUint16(b[54:], 56)
	binary.LittleEndian.PutUint16(b[56:], 3)
	program := func(index int, kind uint32, off, address, size uint64) {
		p := b[64+index*56:]
		binary.LittleEndian.PutUint32(p, kind)
		binary.LittleEndian.PutUint32(p[4:], 4)
		binary.LittleEndian.PutUint64(p[8:], off)
		binary.LittleEndian.PutUint64(p[16:], address)
		binary.LittleEndian.PutUint64(p[32:], size)
		binary.LittleEndian.PutUint64(p[40:], size)
		binary.LittleEndian.PutUint64(p[48:], 4096)
	}
	program(0, 1, 0, 0x400000, 512)
	program(1, 1, 512, 0x400200, 512)
	program(2, 2, 256, 0x400100, 64)
	for i, pair := range [][2]uint64{{5, 0x4001fc}, {10, 64}, {1, 1}, {0, 0}} {
		binary.LittleEndian.PutUint64(b[256+i*16:], pair[0])
		binary.LittleEndian.PutUint64(b[264+i*16:], pair[1])
	}
	copy(b[508:], "\x00libc.so.6\x00")
	return b
}

func dynamicFixture(t *testing.T, b []byte) (map[elf.DynTag][]string, error) {
	t.Helper()
	f, e := elf.NewFile(bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	return elfDynamic(f)
}

func TestSectionlessLoaderViewCrossesContiguousSegments(t *testing.T) {
	got, err := dynamicFixture(t, loaderFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(got[elf.DT_NEEDED]) != 1 || got[elf.DT_NEEDED][0] != "libc.so.6" {
		t.Fatalf("lost loader dependencies: %#v", got)
	}
}

func TestLoaderViewRejectsAmbiguityDiscontinuityAndHiddenExtensions(t *testing.T) {
	cases := map[string]func([]byte){
		"dynamic-offset-disagreement": func(b []byte) { binary.LittleEndian.PutUint64(b[64+2*56+8:], 320) },
		"load-gap":                    func(b []byte) { binary.LittleEndian.PutUint64(b[64+56+16:], 0x400201) },
		"load-overlap":                func(b []byte) { binary.LittleEndian.PutUint64(b[64+56+16:], 0x4001ff) },
		"file-offset-gap":             func(b []byte) { binary.LittleEndian.PutUint64(b[64+56+8:], 513) },
		"filesz-exceeds-memsz":        func(b []byte) { binary.LittleEndian.PutUint64(b[64+40:], 500) },
		"strtab-overflow":             func(b []byte) { binary.LittleEndian.PutUint64(b[264:], ^uint64(0)-10) },
		"dynamic-without-null":        func(b []byte) { binary.LittleEndian.PutUint64(b[304:], 1) },
		"embedded-audit":              func(b []byte) { binary.LittleEndian.PutUint64(b[288:], 0x6ffffefc) },
		"embedded-depaudit":           func(b []byte) { binary.LittleEndian.PutUint64(b[288:], 0x6ffffefb) },
		"embedded-filter":             func(b []byte) { binary.LittleEndian.PutUint64(b[288:], 0x7fffffff) },
		"embedded-auxiliary":          func(b []byte) { binary.LittleEndian.PutUint64(b[288:], 0x7ffffffd) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := loaderFixture()
			mutate(b)
			if _, err := dynamicFixture(t, b); err == nil {
				t.Fatal("unmodeled loader view accepted")
			}
		})
	}
}

func TestOriginNeededStaysInsidePublisherInventory(t *testing.T) {
	prefix := "/prefix/python"
	path := prefix + "/lib/libpython3.13.so.1.0"
	objects := map[string]Entry{path: {Kind: "file"}}
	got, err := originNeeded("$ORIGIN/../lib/libpython3.13.so.1.0", prefix+"/lib/libpython3.so", prefix, objects)
	if err != nil || got != path {
		t.Fatalf("published bounded origin dependency refused: %q %v", got, err)
	}
	for _, bad := range []string{"/outside/lib.so", "$ORIGIN/../../../lib.so", "$LIB/lib.so", "$ORIGIN/$PLATFORM/lib.so", "$ORIGIN/missing.so"} {
		if _, err := originNeeded(bad, prefix+"/lib/object.so", prefix, objects); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestHiddenTarExtensionConsumesFullStreamBudget(t *testing.T) {
	var raw bytes.Buffer
	w := tar.NewWriter(&raw)
	if err := w.WriteHeader(&tar.Header{Name: "python/data", Mode: 0644, Size: 1, Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": strings.Repeat("x", 8192)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&archiveBudget{reader: bytes.NewReader(raw.Bytes()), remaining: 1024})
	if _, err := reader.Next(); err == nil || err == io.EOF {
		t.Fatalf("hidden extension escaped total-byte budget: %v", err)
	}
}
