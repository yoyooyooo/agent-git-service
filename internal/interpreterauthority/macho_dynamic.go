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
		// These commands describe this image's segments, symbols, identity,
		// debug/link-edit metadata, minimum OS or entry point. They introduce
		// neither another image nor a library search path.
		case 0x1, 0x19, // LC_SEGMENT, LC_SEGMENT_64
			0x2, 0xb, // LC_SYMTAB, LC_DYSYMTAB
			0x4, 0x5, // LC_THREAD, LC_UNIXTHREAD
			0xd, 0x1b, // LC_ID_DYLIB, LC_UUID
			0x1d, 0x1e, // LC_CODE_SIGNATURE, LC_SEGMENT_SPLIT_INFO
			0x21, 0x2c, // LC_ENCRYPTION_INFO, LC_ENCRYPTION_INFO_64
			0x24, 0x25, 0x2a, // LC_VERSION_MIN_MACOSX/IPHONEOS, LC_SOURCE_VERSION
			0x26, 0x29, 0x2b, // LC_FUNCTION_STARTS, LC_DATA_IN_CODE, LC_DYLIB_CODE_SIGN_DRS
			0x2d, 0x2e, // LC_LINKER_OPTION, LC_LINKER_OPTIMIZATION_HINT
			0x2f, 0x30, // LC_VERSION_MIN_TVOS, LC_VERSION_MIN_WATCHOS
			0x31, 0x32, // LC_NOTE, LC_BUILD_VERSION
			0x80000028: // LC_MAIN
		// Every other command, including unknown LC_REQ_DYLD commands and
		// unmodeled dyld fixup/export commands, fails closed. A new loader
		// semantic needs an explicit model before catalog admission.
		default:
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
