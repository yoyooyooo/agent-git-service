package interpreterauthority

import (
	"debug/elf"
	"encoding/binary"
	"slices"
	"sort"
	"strings"
)

// A loader-visible range may cross adjacent PT_LOADs (the published PBS
// executable does). Each byte must have one file-backed mapping, with no gap,
// overlap or offset discontinuity. Section headers are not part of this proof.
func elfMappedBytes(image *elf.File, address, size uint64) ([]byte, uint64, error) {
	if size == 0 || size > 16<<20 || address > ^uint64(0)-size || len(image.Progs) > 128 {
		return nil, 0, refusal("authority_elf_mapping_invalid")
	}
	end := address + size
	segments := []*elf.Prog{}
	for _, p := range image.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Filesz > p.Memsz || p.Vaddr > ^uint64(0)-p.Filesz || p.Off > ^uint64(0)-p.Filesz {
			return nil, 0, refusal("authority_elf_mapping_invalid")
		}
		if p.Vaddr < end && address < p.Vaddr+p.Filesz {
			segments = append(segments, p)
		}
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Vaddr < segments[j].Vaddr })
	raw := make([]byte, size)
	cursor, firstOffset := address, uint64(0)
	for index, p := range segments {
		if p.Vaddr > cursor || (index > 0 && p.Vaddr < cursor) {
			return nil, 0, refusal("authority_elf_mapping_ambiguous_or_gap")
		}
		start := cursor - p.Vaddr
		if start >= p.Filesz {
			return nil, 0, refusal("authority_elf_mapping_ambiguous_or_gap")
		}
		count := p.Filesz - start
		if count > end-cursor {
			count = end - cursor
		}
		offset := p.Off + start
		if index == 0 {
			firstOffset = offset
		}
		if firstOffset > ^uint64(0)-(cursor-address) || offset != firstOffset+(cursor-address) {
			return nil, 0, refusal("authority_elf_mapping_discontinuous")
		}
		if _, err := p.ReadAt(raw[cursor-address:cursor-address+count], int64(start)); err != nil {
			return nil, 0, refusal("authority_elf_mapping_unreadable")
		}
		cursor += count
	}
	if cursor != end {
		return nil, 0, refusal("authority_elf_mapping_unmapped")
	}
	return raw, firstOffset, nil
}

// elfDynamic uses the program headers consumed by the native loader. Go's
// elf.DynString reads SHT_DYNAMIC and cannot alone verify a sectionless image.
func elfDynamic(image *elf.File) (map[elf.DynTag][]string, error) {
	var dynamic *elf.Prog
	for _, p := range image.Progs {
		if p.Type == elf.PT_DYNAMIC {
			if dynamic != nil || p.Filesz == 0 || p.Filesz > 1<<20 || p.Filesz%16 != 0 {
				return nil, refusal("authority_elf_dynamic")
			}
			dynamic = p
		}
	}
	out := map[elf.DynTag][]string{}
	if dynamic == nil {
		for _, s := range image.Sections {
			if s.Type == elf.SHT_DYNAMIC {
				return nil, refusal("authority_elf_dynamic_disagreement")
			}
		}
		return out, nil
	}
	if dynamic.Filesz > dynamic.Memsz {
		return nil, refusal("authority_elf_dynamic_unmapped")
	}
	raw, offset, err := elfMappedBytes(image, dynamic.Vaddr, dynamic.Filesz)
	if err != nil || offset != dynamic.Off || uint64(len(raw)) != dynamic.Filesz {
		return nil, refusal("authority_elf_dynamic")
	}
	values := map[elf.DynTag][]uint64{}
	terminated := false
	for pos := 0; pos+16 <= len(raw); pos += 16 {
		tag := elf.DynTag(binary.LittleEndian.Uint64(raw[pos:]))
		value := binary.LittleEndian.Uint64(raw[pos+8:])
		if tag == elf.DT_NULL {
			terminated = true
			break
		}
		// These tags can execute/load additional objects even during --list.
		switch uint64(tag) {
		case 0x6ffffefa, 0x6ffffefb, 0x6ffffefc, 0x7ffffffd, 0x7fffffff:
			return nil, refusal("authority_elf_loader_extension_unsupported")
		}
		values[tag] = append(values[tag], value)
		if tag == elf.DT_FLAGS && value & ^uint64(0x1|0x2|0x8|0x10) != 0 {
			return nil, refusal("authority_elf_flags_unsupported")
		}
		if uint64(tag) == 0x6ffffffb && value & ^uint64(0x1|0x8|0x80|0x08000000) != 0 {
			return nil, refusal("authority_elf_flags_unsupported")
		}
	}
	if !terminated {
		return nil, refusal("authority_elf_dynamic_unterminated")
	}
	for _, tag := range []elf.DynTag{elf.DT_STRTAB, elf.DT_STRSZ, elf.DT_SONAME, elf.DT_RPATH, elf.DT_RUNPATH, elf.DT_FLAGS, elf.DT_FLAGS_1} {
		if len(values[tag]) > 1 {
			return nil, refusal("authority_elf_dynamic_duplicate")
		}
	}
	if len(values[elf.DT_STRTAB]) != 1 || len(values[elf.DT_STRSZ]) != 1 {
		return nil, refusal("authority_elf_string_table_missing")
	}
	address, size := values[elf.DT_STRTAB][0], values[elf.DT_STRSZ][0]
	if size == 0 || size > 16<<20 {
		return nil, refusal("authority_elf_string_table_size")
	}
	stringsRaw, _, err := elfMappedBytes(image, address, size)
	if err != nil {
		return nil, err
	}
	for _, tag := range []elf.DynTag{elf.DT_NEEDED, elf.DT_SONAME, elf.DT_RPATH, elf.DT_RUNPATH} {
		for _, offset := range values[tag] {
			if offset >= uint64(len(stringsRaw)) {
				return nil, refusal("authority_elf_string_offset")
			}
			part := stringsRaw[offset:]
			end := strings.IndexByte(string(part), 0)
			if end < 0 || end > 4096 {
				return nil, refusal("authority_elf_string_termination")
			}
			value := string(part[:end])
			if !cleanText(value) {
				return nil, refusal("authority_elf_string_invalid")
			}
			out[tag] = append(out[tag], value)
		}
	}
	// Existing sections may corroborate the loader view, never replace it.
	for _, s := range image.Sections {
		if s.Type != elf.SHT_DYNAMIC {
			continue
		}
		if s.Offset != dynamic.Off || s.Size != dynamic.Filesz {
			return nil, refusal("authority_elf_dynamic_disagreement")
		}
		for _, tag := range []elf.DynTag{elf.DT_NEEDED, elf.DT_SONAME, elf.DT_RPATH, elf.DT_RUNPATH} {
			got, err := image.DynString(tag)
			if err != nil || !slices.Equal(got, out[tag]) {
				return nil, refusal("authority_elf_dynamic_disagreement")
			}
		}
	}
	return out, nil
}
