// Package interpreterauthority verifies a separately registered interpreter
// catalog without executing its interpreter or changing the filesystem.
// Its evidence is not permission to start a native transaction.
package interpreterauthority

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

const (
	Schema      = "ags.interpreter-authority.v1"
	Registry    = "/usr/local/libexec/ags-routeb-interpreters"
	MaxDocument = 16 << 20
	MaxArchive  = 256 << 20
	MaxFile     = 128 << 20
	MaxExpanded = 512 << 20
	MaxMembers  = 16384
)

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)
	digest     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	oid        = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Binding comes from the independently registered host/tool context. It is
// deliberately not an execution-proof token or a caller-selectable verifier.
type Binding struct {
	HostInstance       string `json:"host_instance"`
	HostProfileSHA256  string `json:"host_profile_sha256"`
	ToolRevision       string `json:"tool_revision"`
	ToolTree           string `json:"tool_tree"`
	ToolManifestSHA256 string `json:"tool_manifest_sha256"`
	Platform           string `json:"platform"`
	Architecture       string `json:"architecture"`
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Metadata struct {
	Mode    uint32            `json:"mode"`
	GID     uint32            `json:"gid"`
	Device  uint64            `json:"device"`
	Inode   uint64            `json:"inode"`
	MtimeNS int64             `json:"mtime_ns"`
	Xattrs  map[string]string `json:"xattrs"`
}

type Content struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Entry struct {
	Path     string   `json:"path"`
	Kind     string   `json:"kind"`
	Metadata Metadata `json:"metadata"`
	File     *Content `json:"file,omitempty"`
	Link     *string  `json:"link,omitempty"`
}

type Tree struct {
	Root    string  `json:"root"`
	Entries []Entry `json:"entries"`
}

type Publisher struct {
	Repository string `json:"repository"`
	Release    string `json:"release"`
	AssetName  string `json:"asset_name"`
	Archive    File   `json:"archive"`
	Checksums  File   `json:"checksums"`
}

type External struct {
	Entry Entry `json:"entry"`
	// A regular external file has an independent sealed recovery copy.
	Asset *File `json:"asset,omitempty"`
}

type SearchPath struct {
	Path    string   `json:"path"`
	State   string   `json:"state"`
	Members []string `json:"members"`
}

type NativeImage struct {
	Path        string            `json:"path"`
	Format      string            `json:"format"`
	Interpreter string            `json:"interpreter"`
	Needed      []string          `json:"needed"`
	RPath       []string          `json:"rpath"`
	RunPath     []string          `json:"runpath"`
	Resolution  map[string]string `json:"resolution"`
}

// LinuxEntry fixes a loader invocation with no cache or hardware-capability
// directory selection. Runtime library copies have ordinary External entries
// and distinct sealed recovery assets; the live copies are not their own backup.
type LinuxEntry struct {
	Loader           string `json:"loader"`
	LibraryDirectory string `json:"library_directory"`
}

type Document struct {
	Schema       string            `json:"schema"`
	ID           string            `json:"id"`
	Binding      Binding           `json:"binding"`
	Publisher    Publisher         `json:"publisher"`
	Prefix       Tree              `json:"prefix"`
	Executable   string            `json:"executable"`
	External     []External        `json:"external"`
	SearchPaths  []SearchPath      `json:"search_paths"`
	NativeImages []NativeImage     `json:"native_images"`
	Environment  map[string]string `json:"environment"`
	Linux        *LinuxEntry       `json:"linux,omitempty"`
}

func refusal(code string) error { return errors.New(code) }

func ValidID(value string) bool {
	return identifier.MatchString(value) && value != "." && value != ".."
}
func validDigest(value string) bool {
	return digest.MatchString(value) && value != strings.Repeat("0", 64)
}
func validOID(value string) bool { return oid.MatchString(value) && value != strings.Repeat("0", 40) }

func cleanText(value string) bool {
	return value != "" && !strings.ContainsAny(value, "\\\x00\r\n\t")
}

func absolute(value string) bool {
	return cleanText(value) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func relative(value string) bool {
	return cleanText(value) && value != "." && !path.IsAbs(value) && path.Clean(value) == value &&
		value != ".." && !strings.HasPrefix(value, "../")
}

// StrictJSON refuses duplicate, missing and unknown fields, including fields
// whose legitimate value is zero. json.Decoder alone would accept duplicates
// and would silently supply zero values for omitted fields.
func StrictJSON(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > MaxDocument {
		return refusal("authority_document_size")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := uniqueValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return refusal("authority_json_trailing")
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return refusal("authority_json_shape")
	}
	var shape any
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&shape); err != nil {
		return refusal("authority_json_shape")
	}
	return requiredFields(shape, reflect.TypeOf(target).Elem())
}

func uniqueValue(dec *json.Decoder, depth int) error {
	if depth > 32 {
		return refusal("authority_json_depth")
	}
	token, err := dec.Token()
	if err != nil {
		return refusal("authority_json_invalid")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			token, err = dec.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return refusal("authority_json_duplicate")
			}
			seen[key] = true
			if err = uniqueValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err = uniqueValue(dec, depth+1); err != nil {
				return err
			}
		}
	default:
		return refusal("authority_json_invalid")
	}
	end, err := dec.Token()
	if err != nil || (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
		return refusal("authority_json_invalid")
	}
	return nil
}

func requiredFields(value any, typ reflect.Type) error {
	if typ.Kind() == reflect.Pointer {
		if value == nil {
			return refusal("authority_json_null")
		}
		return requiredFields(value, typ.Elem())
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return refusal("authority_json_shape")
		}
		allowed := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			allowed[name] = true
		}
		for key := range object {
			if !allowed[key] {
				return refusal("authority_json_unknown")
			}
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			name := tag[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			child, exists := object[name]
			if !exists {
				if len(tag) > 1 && tag[1] == "omitempty" {
					continue
				}
				return refusal("authority_json_missing")
			}
			if err := requiredFields(child, field.Type); err != nil {
				return err
			}
		}
	case reflect.Slice:
		array, ok := value.([]any)
		if !ok {
			return refusal("authority_json_shape")
		}
		for _, child := range array {
			if err := requiredFields(child, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return refusal("authority_json_shape")
		}
		for _, child := range object {
			if err := requiredFields(child, typ.Elem()); err != nil {
				return err
			}
		}
	default:
		if value == nil {
			return refusal("authority_json_null")
		}
	}
	return nil
}

func Parse(raw []byte) (Document, error) {
	var value Document
	if err := StrictJSON(raw, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

func (binding Binding) Validate() error {
	if !ValidID(binding.HostInstance) || !validDigest(binding.HostProfileSHA256) ||
		!validOID(binding.ToolRevision) || !validOID(binding.ToolTree) || !validDigest(binding.ToolManifestSHA256) ||
		(binding.Platform != "linux" && binding.Platform != "darwin") ||
		(binding.Architecture != "amd64" && binding.Architecture != "arm64") {
		return refusal("authority_binding_invalid")
	}
	return nil
}

func (file File) validate(maximum int64) error {
	if !absolute(file.Path) || !validDigest(file.SHA256) || file.Size < 0 || file.Size > maximum {
		return refusal("authority_file_invalid")
	}
	return nil
}

func (entry Entry) validate(isAbsolute bool) error {
	if (isAbsolute && !absolute(entry.Path)) || (!isAbsolute && entry.Path != "." && !relative(entry.Path)) ||
		entry.Metadata.Mode > 0777 || entry.Metadata.Device == 0 || entry.Metadata.Inode == 0 ||
		entry.Metadata.MtimeNS < 0 || entry.Metadata.Xattrs == nil {
		return refusal("authority_entry_invalid")
	}
	for key, value := range entry.Metadata.Xattrs {
		if !cleanText(key) || !validDigest(value) || strings.Contains(key, "posix_acl") || key == "security.capability" {
			return refusal("authority_metadata_unsupported")
		}
	}
	switch entry.Kind {
	case "file":
		if entry.File == nil || entry.Link != nil || entry.File.Size < 0 || entry.File.Size > MaxFile ||
			!validDigest(entry.File.SHA256) || entry.Metadata.Mode&0022 != 0 {
			return refusal("authority_file_invalid")
		}
	case "directory":
		if entry.File != nil || entry.Link != nil || entry.Metadata.Mode&0022 != 0 {
			return refusal("authority_directory_invalid")
		}
	case "symlink":
		if entry.File != nil || entry.Link == nil || !cleanText(*entry.Link) || entry.Metadata.Mode != 0777 {
			return refusal("authority_link_invalid")
		}
	default:
		return refusal("authority_kind_invalid")
	}
	return nil
}

func (value Document) Validate() error {
	if value.Schema != Schema || !ValidID(value.ID) || !absolute(value.Prefix.Root) ||
		!relative(value.Executable) || len(value.Prefix.Entries) == 0 || len(value.Prefix.Entries) > MaxMembers ||
		len(value.External) > 1024 || len(value.SearchPaths) > 128 || len(value.NativeImages) > 256 {
		return refusal("authority_invalid")
	}
	if err := value.Binding.Validate(); err != nil {
		return err
	}
	if value.Linux != nil && (value.Binding.Platform != "linux" || !absolute(value.Linux.Loader) ||
		value.Linux.LibraryDirectory != filepath.Join(Registry, value.ID, "runtime-libraries")) {
		return refusal("authority_linux_entry_invalid")
	}
	if len(value.Environment) != 2 || value.Environment["LANG"] != "C.UTF-8" || value.Environment["LC_ALL"] != "C.UTF-8" {
		return refusal("authority_environment_invalid")
	}
	pub := value.Publisher
	if pub.Repository != "astral-sh/python-build-standalone" || !regexp.MustCompile(`^[0-9]{8}$`).MatchString(pub.Release) ||
		!relative(pub.AssetName) || path.Base(pub.AssetName) != pub.AssetName || !strings.HasSuffix(pub.AssetName, "-install_only_stripped.tar.gz") {
		return refusal("authority_publisher_invalid")
	}
	if err := pub.Archive.validate(MaxArchive); err != nil {
		return err
	}
	if err := pub.Checksums.validate(MaxDocument); err != nil {
		return err
	}
	if pub.Archive.Path == pub.Checksums.Path || within(pub.Archive.Path, value.Prefix.Root) || within(pub.Checksums.Path, value.Prefix.Root) {
		return refusal("authority_asset_not_independent")
	}
	entries := map[string]Entry{}
	var total int64
	for _, entry := range value.Prefix.Entries {
		if err := entry.validate(false); err != nil {
			return err
		}
		if _, exists := entries[entry.Path]; exists {
			return refusal("authority_duplicate_path")
		}
		for name := range entry.Metadata.Xattrs {
			if value.Binding.Platform != "darwin" || name != "com.apple.provenance" {
				return refusal("authority_prefix_metadata_unsupported")
			}
		}
		entries[entry.Path] = entry
		if entry.File != nil {
			total += entry.File.Size
			if total > MaxExpanded {
				return refusal("authority_expanded_size")
			}
		}
	}
	if entries["."].Kind != "directory" || entries[value.Executable].Kind != "file" || entries[value.Executable].Metadata.Mode&0111 == 0 {
		return refusal("authority_executable_invalid")
	}
	for name, entry := range entries {
		if name != "." && entries[path.Dir(name)].Kind != "directory" {
			return refusal("authority_parent_missing")
		}
		if entry.Kind == "symlink" {
			if _, err := resolveLink(name, entries, false); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{}
	for _, external := range value.External {
		e := external.Entry
		if err := e.validate(true); err != nil {
			return err
		}
		if seen[e.Path] || within(e.Path, value.Prefix.Root) {
			return refusal("authority_duplicate_path")
		}
		seen[e.Path] = true
		if e.Kind == "file" {
			if external.Asset == nil {
				return refusal("authority_recovery_asset_missing")
			}
			if err := external.Asset.validate(MaxFile); err != nil {
				return err
			}
			if external.Asset.Path == e.Path || within(external.Asset.Path, value.Prefix.Root) ||
				external.Asset.SHA256 != e.File.SHA256 || external.Asset.Size != e.File.Size {
				return refusal("authority_recovery_asset_mismatch")
			}
		} else if external.Asset != nil {
			return refusal("authority_recovery_asset_invalid")
		}
	}
	seen = map[string]bool{}
	for _, search := range value.SearchPaths {
		if !absolute(search.Path) || seen[search.Path] || within(search.Path, value.Prefix.Root) ||
			(search.State != "absent" && search.State != "closed-directory") {
			return refusal("authority_search_path_invalid")
		}
		seen[search.Path] = true
		members := map[string]bool{}
		for _, name := range search.Members {
			if !relative(name) || path.Base(name) != name || members[name] {
				return refusal("authority_search_path_invalid")
			}
			members[name] = true
		}
		if search.State == "absent" && len(search.Members) != 0 {
			return refusal("authority_search_path_invalid")
		}
	}
	seen = map[string]bool{}
	for _, image := range value.NativeImages {
		if !absolute(image.Path) || seen[image.Path] || (image.Format != "elf" && image.Format != "macho") {
			return refusal("authority_native_image_invalid")
		}
		seen[image.Path] = true
		if image.Interpreter != "" && !absolute(image.Interpreter) {
			return refusal("authority_native_image_invalid")
		}
		if len(image.Resolution) != len(image.Needed) {
			return refusal("authority_native_resolution_incomplete")
		}
		needed := map[string]bool{}
		for _, name := range image.Needed {
			if !cleanText(name) || needed[name] || !absolute(image.Resolution[name]) {
				return refusal("authority_native_resolution_incomplete")
			}
			needed[name] = true
		}
	}
	return nil
}

func within(child, root string) bool { return child == root || strings.HasPrefix(child, root+"/") }

func resolveLink(name string, entries map[string]Entry, absoluteLinks bool) (string, error) {
	seen := map[string]bool{}
	for step := 0; step < 64; step++ {
		entry, found := entries[name]
		if !found {
			return "", refusal("authority_link_dangling")
		}
		if entry.Kind != "symlink" {
			return name, nil
		}
		if seen[name] {
			return "", refusal("authority_link_cycle")
		}
		seen[name] = true
		target := *entry.Link
		if path.IsAbs(target) {
			if !absoluteLinks {
				return "", refusal("authority_link_escape")
			}
			name = path.Clean(target)
		} else {
			name = path.Clean(path.Join(path.Dir(name), target))
		}
		if !absoluteLinks && !relative(name) && name != "." {
			return "", refusal("authority_link_escape")
		}
	}
	return "", fmt.Errorf("authority_link_depth")
}
