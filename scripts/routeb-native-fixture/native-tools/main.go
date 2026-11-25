// Command native-tools prepares only fresh fixture data and inspects real ELF
// inputs. It is not an application launcher or a native admission substitute.
package main

import (
	"debug/elf"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These fixed catalog refusal codes contain no input values. Keep unknown
// errors opaque even when their text resembles an authority_* identifier.
var safeFailureReasons = func() map[string]bool {
	result := map[string]bool{}
	for _, code := range strings.Fields(`
already_exists authority_absence_changed authority_acl_unsupported authority_ancestor_not_directory
authority_ancestor_unavailable authority_asset_changed authority_asset_not_independent
authority_asset_unreadable authority_binding_invalid authority_binding_mismatch
authority_directory_invalid authority_document_changed authority_document_size
authority_duplicate_path authority_elf_dynamic authority_elf_dynamic_disagreement
authority_elf_dynamic_duplicate authority_elf_dynamic_unmapped authority_elf_dynamic_unterminated
authority_elf_flags_unsupported authority_elf_interpreter authority_elf_invalid
authority_elf_loader_extension_unsupported authority_elf_mapping_ambiguous_or_gap
authority_elf_mapping_discontinuous authority_elf_mapping_invalid authority_elf_mapping_unmapped
authority_elf_mapping_unreadable authority_elf_string_invalid authority_elf_string_offset
authority_elf_string_table_missing authority_elf_string_table_size authority_elf_string_termination
authority_entry_invalid authority_environment_invalid authority_executable_invalid
authority_executable_not_native authority_expanded_size authority_fat_image_unsupported
authority_file_invalid authority_file_unavailable authority_file_unsafe authority_invalid
authority_inventory_unavailable authority_json_depth authority_json_duplicate authority_json_invalid
authority_json_missing authority_json_null authority_json_shape authority_json_trailing
authority_json_unknown authority_kind_invalid authority_link_changed authority_link_cycle
authority_link_dangling authority_link_depth authority_link_escape authority_link_invalid
authority_linux_entry_invalid authority_linux_entry_required authority_linux_external_rpath
authority_linux_library_member authority_linux_loader_missing authority_linux_needed_soname
authority_linux_needed_unbound authority_linux_needed_unsupported authority_linux_search_unbound
authority_linux_search_unsupported authority_macho_invalid authority_macho_loader_unsupported
authority_member_changed authority_member_count authority_member_kind_changed
authority_member_unavailable authority_metadata_changed authority_metadata_unavailable
authority_metadata_unsupported authority_native_architecture authority_native_dependency_missing
authority_native_image_changed authority_native_image_invalid authority_native_image_set
authority_native_image_unavailable authority_native_loader_configuration_missing
authority_native_loader_missing authority_native_needed_path authority_native_platform
authority_native_resolution_incomplete authority_native_search_unbound
authority_native_search_unsupported authority_native_soname_mismatch authority_parent_missing
authority_path_outside_boundary authority_path_untrusted authority_prefix_member_set
authority_prefix_metadata_unsupported authority_prefix_unavailable authority_publisher_invalid
authority_read_limit authority_recovery_asset_invalid authority_recovery_asset_mismatch
authority_recovery_asset_missing authority_reference_invalid authority_search_members_changed
authority_search_path_invalid authority_search_unavailable authority_xattrs_changed catalog_error
database_error elf_architecture elf_interpreter end_of_file filesystem_error fixture_command
fixture_database_path invalid_elf invalid_json json_type_mismatch not_found operation_error
output_encoding permission_denied publisher_archive_changed publisher_archive_invalid
publisher_archive_stream_limit publisher_archive_trailing publisher_checksum_mismatch
publisher_checksum_missing_or_duplicate publisher_checksums_unreadable publisher_expanded_size
publisher_link_invalid publisher_member_collision publisher_member_count publisher_member_path
publisher_member_size publisher_member_unsupported publisher_parent_invalid
publisher_prefix_content_mismatch publisher_prefix_link_mismatch publisher_prefix_member_mismatch
publisher_prefix_member_set unexpected_eof
`) {
		result[code] = true
	}
	return result
}()

type nativeFailure struct {
	Schema string `json:"schema"`
	Stage  string `json:"stage"`
	Reason string `json:"reason"`
}

func diagnosticFailure(stage string, err error) nativeFailure {
	switch stage {
	case "inspect-elf", "verify-catalog", "seed-db", "encode-result":
	default:
		stage = "dispatch"
	}
	reason := "operation_error"
	var pathError *os.PathError
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	var elfError *elf.FormatError
	switch {
	case safeFailureReasons[err.Error()]:
		reason = err.Error()
	case errors.Is(err, os.ErrPermission):
		reason = "permission_denied"
	case errors.Is(err, os.ErrNotExist):
		reason = "not_found"
	case errors.Is(err, os.ErrExist):
		reason = "already_exists"
	case errors.Is(err, io.ErrUnexpectedEOF):
		reason = "unexpected_eof"
	case errors.Is(err, io.EOF):
		reason = "end_of_file"
	case errors.As(err, &syntaxError):
		reason = "invalid_json"
	case errors.As(err, &typeError):
		reason = "json_type_mismatch"
	case errors.As(err, &elfError):
		reason = "invalid_elf"
	case errors.As(err, &pathError):
		reason = "filesystem_error"
	case stage == "seed-db":
		reason = "database_error"
	case stage == "verify-catalog":
		reason = "catalog_error"
	case stage == "encode-result":
		reason = "output_encoding"
	}
	return nativeFailure{Schema: "ags.routeb-fixture-command-error.v1", Stage: stage, Reason: reason}
}

func inspectELF(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err = io.ReadFull(f, magic); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
		return map[string]any{"native": false}, nil
	}
	if string(magic) != "\x7fELF" {
		return map[string]any{"native": false}, nil
	}
	image, err := elf.NewFile(f)
	if err != nil {
		return nil, err
	}
	if image.Class != elf.ELFCLASS64 || image.Data != elf.ELFDATA2LSB || image.Machine != elf.EM_X86_64 {
		return nil, errors.New("elf_architecture")
	}
	stringsFor := func(tag elf.DynTag) ([]string, error) {
		values, err := image.DynString(tag)
		if values == nil {
			values = []string{}
		}
		return values, err
	}
	needed, err := stringsFor(elf.DT_NEEDED)
	if err != nil {
		return nil, err
	}
	rpath, err := stringsFor(elf.DT_RPATH)
	if err != nil {
		return nil, err
	}
	runpath, err := stringsFor(elf.DT_RUNPATH)
	if err != nil {
		return nil, err
	}
	interpreter := ""
	for _, program := range image.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(program.Open(), 4097))
		if err != nil || len(raw) < 2 || len(raw) > 4096 || raw[len(raw)-1] != 0 || interpreter != "" {
			return nil, errors.New("elf_interpreter")
		}
		interpreter = string(raw[:len(raw)-1])
	}
	return map[string]any{"native": true, "image": interpreterauthority.NativeImage{
		Path: path, Format: "elf", Interpreter: interpreter, Needed: needed,
		RPath: rpath, RunPath: runpath, Resolution: map[string]string{},
	}}, nil
}

func seed(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "main.db" ||
		!strings.Contains(path, "/.local/state/ags-routeb-native/") || os.Getuid() == 0 {
		return errors.New("fixture_database_path")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	database, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return err
	}
	sqlDB, err := database.DB()
	if err != nil {
		return err
	}
	migrateErr := db.Migrate(database)
	closeErr := sqlDB.Close()
	return errors.Join(migrateErr, closeErr)
}

func run(args []string) (any, error) {
	if len(args) == 2 && args[0] == "inspect-elf" {
		return inspectELF(args[1])
	}
	if len(args) == 2 && args[0] == "seed-db" {
		if err := seed(args[1]); err != nil {
			return nil, err
		}
		return map[string]any{"schema": "ags.routeb-fixture-seed.v1", "status": "migrated", "native_pass": false}, nil
	}
	if len(args) == 2 && args[0] == "verify-catalog" {
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return nil, err
		}
		var input struct {
			ID      string                       `json:"id"`
			SHA256  string                       `json:"sha256"`
			Binding interpreterauthority.Binding `json:"binding"`
		}
		if err = interpreterauthority.StrictJSON(raw, &input); err != nil {
			return nil, err
		}
		return interpreterauthority.VerifyRegistered(input.ID, input.SHA256, input.Binding)
	}
	return nil, errors.New("fixture_command")
}

func main() {
	value, err := run(os.Args[1:])
	if err != nil {
		stage := "dispatch"
		if len(os.Args) > 1 {
			stage = os.Args[1]
		}
		_ = json.NewEncoder(os.Stderr).Encode(diagnosticFailure(stage, err))
		os.Exit(78)
	}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(diagnosticFailure("encode-result", err))
		os.Exit(78)
	}
}
