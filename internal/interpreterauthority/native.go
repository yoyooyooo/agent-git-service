package interpreterauthority

import (
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// On-disk fat/universal markers. The CIGAM values are byte-swapped forms.
// They are refused without treating that layout as a supported load format.
const (
	fatMagic64 = 0xcafebabf
	fatCigam   = 0xbebafeca
	fatCigam64 = 0xbfbafeca
)

type observedImage struct {
	format                 string
	interpreter            string
	needed, rpath, runpath []string
	soname                 string
}

func inspectNative(name, architecture string) (observedImage, bool, error) {
	file, err := os.Open(name)
	if err != nil {
		return observedImage{}, false, refusal("authority_native_image_unavailable")
	}
	defer file.Close()
	magic := make([]byte, 4)
	if _, err = io.ReadFull(file, magic); err == io.EOF || err == io.ErrUnexpectedEOF {
		return observedImage{}, false, nil
	}
	if err != nil {
		return observedImage{}, false, refusal("authority_native_image_unavailable")
	}
	if string(magic) == "\x7fELF" {
		image, err := elf.NewFile(file)
		if err != nil {
			return observedImage{}, true, refusal("authority_elf_invalid")
		}
		expected := elf.EM_X86_64
		if architecture == "arm64" {
			expected = elf.EM_AARCH64
		}
		if image.Class != elf.ELFCLASS64 || image.Data != elf.ELFDATA2LSB || image.Machine != expected {
			return observedImage{}, true, refusal("authority_native_architecture")
		}
		result := observedImage{format: "elf"}
		dynamic, err := elfDynamic(image)
		if err != nil {
			return result, true, err
		}
		result.needed, result.rpath, result.runpath = dynamic[elf.DT_NEEDED], dynamic[elf.DT_RPATH], dynamic[elf.DT_RUNPATH]
		sonames := dynamic[elf.DT_SONAME]
		if len(sonames) > 1 {
			return result, true, refusal("authority_elf_dynamic")
		}
		if len(sonames) == 1 {
			result.soname = sonames[0]
		}
		for _, program := range image.Progs {
			if program.Type != elf.PT_INTERP {
				continue
			}
			if result.interpreter != "" || program.Filesz == 0 || program.Filesz > 4096 {
				return result, true, refusal("authority_elf_interpreter")
			}
			raw, err := io.ReadAll(io.LimitReader(program.Open(), 4097))
			if err != nil || len(raw) == 0 || raw[len(raw)-1] != 0 || strings.ContainsRune(string(raw[:len(raw)-1]), 0) {
				return result, true, refusal("authority_elf_interpreter")
			}
			result.interpreter = string(raw[:len(raw)-1])
		}
		return result, true, nil
	}
	little, big := binary.LittleEndian.Uint32(magic), binary.BigEndian.Uint32(magic)
	if little == macho.Magic64 || little == macho.Magic32 || big == macho.Magic64 || big == macho.Magic32 {
		image, err := macho.NewFile(file)
		if err != nil {
			return observedImage{}, true, refusal("authority_macho_invalid")
		}
		expected := macho.CpuAmd64
		if architecture == "arm64" {
			expected = macho.CpuArm64
		}
		if image.Cpu != expected {
			return observedImage{}, true, refusal("authority_native_architecture")
		}
		result := observedImage{format: "macho"}
		result.needed, result.rpath, err = machoDynamic(image)
		if err != nil {
			return result, true, err
		}
		return result, true, nil
	}
	switch binary.BigEndian.Uint32(magic) {
	case macho.MagicFat, fatMagic64, fatCigam, fatCigam64:
		return observedImage{}, true, refusal("authority_fat_image_unsupported")
	}
	return observedImage{}, false, nil
}

func resolveObject(name string, objects map[string]Entry) (string, error) {
	for round := 0; round < 64; round++ {
		changed := false
		parts := strings.Split(strings.TrimPrefix(name, "/"), "/")
		current := ""
		for index, part := range parts {
			current += "/" + part
			entry, found := objects[current]
			if !found || entry.Kind != "symlink" {
				continue
			}
			target := *entry.Link
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(current), target)
			}
			name = filepath.Join(append([]string{target}, parts[index+1:]...)...)
			changed = true
			break
		}
		if !changed {
			if objects[name].Kind != "file" {
				return "", refusal("authority_native_dependency_missing")
			}
			return name, nil
		}
	}
	return "", refusal("authority_link_cycle")
}

func verifyNativeImages(value Document) error {
	objects := map[string]Entry{}
	for _, entry := range value.Prefix.Entries {
		objects[filepath.Join(value.Prefix.Root, entry.Path)] = entry
	}
	for _, entry := range value.External {
		objects[entry.Entry.Path] = entry.Entry
	}
	observed := map[string]observedImage{}
	for name, entry := range objects {
		if entry.Kind != "file" {
			continue
		}
		image, native, err := inspectNative(name, value.Binding.Architecture)
		if err != nil {
			return err
		}
		if native {
			observed[name] = image
		}
	}
	if len(observed) != len(value.NativeImages) {
		return refusal("authority_native_image_set")
	}
	executable := filepath.Join(value.Prefix.Root, value.Executable)
	if _, ok := observed[executable]; !ok {
		return refusal("authority_executable_not_native")
	}
	search := map[string]SearchPath{}
	for _, item := range value.SearchPaths {
		search[item.Path] = item
	}
	for _, expected := range value.NativeImages {
		actual, found := observed[expected.Path]
		if !found || actual.format != expected.Format || actual.interpreter != expected.Interpreter ||
			!slices.Equal(actual.needed, expected.Needed) || !slices.Equal(actual.rpath, expected.RPath) || !slices.Equal(actual.runpath, expected.RunPath) {
			return refusal("authority_native_image_changed")
		}
		if (value.Binding.Platform == "linux" && actual.format != "elf") || (value.Binding.Platform == "darwin" && actual.format != "macho") {
			return refusal("authority_native_platform")
		}
		if actual.interpreter != "" {
			target, err := resolveObject(actual.interpreter, objects)
			if err != nil {
				return err
			}
			if _, ok := observed[target]; !ok {
				return refusal("authority_native_loader_missing")
			}
		}
		for _, needed := range actual.needed {
			target, err := resolveObject(expected.Resolution[needed], objects)
			if err != nil {
				return err
			}
			dependency, ok := observed[target]
			if !ok || dependency.format != actual.format {
				return refusal("authority_native_dependency_missing")
			}
			if actual.format == "elf" {
				if strings.Contains(needed, "/") || strings.Contains(needed, "$") {
					exact, err := originNeeded(needed, expected.Path, value.Prefix.Root, objects)
					if err != nil || exact != target {
						return refusal("authority_native_needed_path")
					}
				} else if (dependency.soname != "" && dependency.soname != needed) || (dependency.soname == "" && filepath.Base(target) != needed) {
					return refusal("authority_native_soname_mismatch")
				}
			}
		}
		for _, group := range append(append([]string{}, actual.rpath...), actual.runpath...) {
			for _, item := range strings.Split(group, ":") {
				if item == "" {
					return refusal("authority_native_search_unsupported")
				}
				item = strings.ReplaceAll(strings.ReplaceAll(item, "${ORIGIN}", filepath.Dir(expected.Path)), "$ORIGIN", filepath.Dir(expected.Path))
				if strings.Contains(item, "$") || strings.HasPrefix(item, "@") || !filepath.IsAbs(item) {
					return refusal("authority_native_search_unsupported")
				}
				item = filepath.Clean(item)
				if within(item, value.Prefix.Root) {
					if objects[item].Kind != "directory" {
						return refusal("authority_native_search_unbound")
					}
				} else if _, ok := search[item]; !ok {
					return refusal("authority_native_search_unbound")
				}
			}
		}
		if actual.format == "elf" && (len(actual.needed) > 0 || actual.interpreter != "") {
			if (value.Linux == nil && objects["/etc/ld.so.cache"].Kind != "file") || search["/etc/ld.so.preload"].State != "absent" {
				return refusal("authority_native_loader_configuration_missing")
			}
		}
	}
	return nil
}

func originNeeded(name, image, prefix string, objects map[string]Entry) (string, error) {
	if !strings.HasPrefix(name, "$ORIGIN/") && !strings.HasPrefix(name, "${ORIGIN}/") {
		return "", refusal("authority_native_needed_path")
	}
	expanded := strings.ReplaceAll(strings.ReplaceAll(name, "${ORIGIN}", filepath.Dir(image)), "$ORIGIN", filepath.Dir(image))
	if strings.Contains(expanded, "$") || !filepath.IsAbs(expanded) {
		return "", refusal("authority_native_needed_path")
	}
	expanded = filepath.Clean(expanded)
	if !within(expanded, prefix) {
		return "", refusal("authority_native_needed_path")
	}
	resolved, err := resolveObject(expanded, objects)
	if err != nil || !within(resolved, prefix) {
		return "", refusal("authority_native_needed_path")
	}
	return resolved, nil
}
