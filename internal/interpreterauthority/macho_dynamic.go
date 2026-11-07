package interpreterauthority

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"unicode/utf8"
)

// machoDynamic reads the loader's library commands, not just the subset that
// debug/macho exposes as *macho.Dylib. Weak, re-exported, lazy and upward loads
// still require an exact catalog entry; missing weak libraries are not admitted.
// This does not model dyld shared-cache authority or authorize native execution.
func machoDynamic(image *macho.File) (needed, rpath []string, err error) {
	order := image.ByteOrder
	if order != binary.LittleEndian && order != binary.BigEndian {
		return nil, nil, refusal("authority_macho_invalid")
	}
	alignment := 4
	switch image.Magic {
	case macho.Magic64:
		alignment = 8
	case macho.Magic32:
	default:
		return nil, nil, refusal("authority_macho_invalid")
	}
	for _, load := range image.Loads {
		raw := load.Raw()
		if len(raw) < 8 || len(raw)%alignment != 0 || uint64(order.Uint32(raw[4:8])) != uint64(len(raw)) {
			return nil, nil, refusal("authority_macho_invalid")
		}
		kind := order.Uint32(raw[:4])
		switch kind {
		// LC_LOAD_DYLIB, LC_LOAD_WEAK_DYLIB, LC_REEXPORT_DYLIB,
		// LC_LAZY_LOAD_DYLIB and LC_LOAD_UPWARD_DYLIB share dylib_command.
		case 0xc, 0x80000018, 0x8000001f, 0x20, 0x80000023:
			name, err := machoCommandString(raw, order, 24)
			if err != nil {
				return nil, nil, err
			}
			needed = append(needed, name)
		case 0x8000001c: // LC_RPATH
			path, err := machoCommandString(raw, order, 12)
			if err != nil {
				return nil, nil, err
			}
			rpath = append(rpath, path)
		// Legacy fixed-VM/prebound loads and embedded dyld environment are
		// outside this catalog's dependency and search-path model.
		case 0x6, 0x9, 0x10, 0x27:
			return nil, nil, refusal("authority_macho_loader_unsupported")
		}
	}
	return needed, rpath, nil
}

func machoCommandString(raw []byte, order binary.ByteOrder, headerSize uint32) (string, error) {
	if uint64(len(raw)) < uint64(headerSize) {
		return "", refusal("authority_macho_invalid")
	}
	offset := order.Uint32(raw[8:12])
	if offset < headerSize || uint64(offset) >= uint64(len(raw)) {
		return "", refusal("authority_macho_invalid")
	}
	value := raw[offset:]
	end := bytes.IndexByte(value, 0)
	if end <= 0 || !utf8.Valid(value[:end]) {
		return "", refusal("authority_macho_invalid")
	}
	return string(value[:end]), nil
}
