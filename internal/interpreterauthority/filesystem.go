package interpreterauthority

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// fsPolicy is internal. Production always checks root ownership from /.
// Tests use a private boundary with their own UID to exercise real filesystem
// tampering without obtaining host provisioning authority.
type fsPolicy struct {
	uid      uint32
	boundary string
}

type Evidence struct {
	Schema                 string  `json:"schema"`
	AuthorityID            string  `json:"authority_id"`
	AuthoritySHA256        string  `json:"authority_sha256"`
	Binding                Binding `json:"binding"`
	Prefix                 string  `json:"prefix"`
	Executable             string  `json:"executable"`
	PrefixMembers          int     `json:"prefix_members"`
	ExternalMembers        int     `json:"external_members"`
	NativeImages           int     `json:"native_images"`
	PublisherArchiveSHA256 string  `json:"publisher_archive_sha256"`
	CatalogVerified        bool    `json:"catalog_verified"`
	NativeExecutionAllowed bool    `json:"native_execution_allowed"`
}

// VerifyRegistered reads only the fixed registry. The expected digest/binding
// must come from the independently root-registered host, not CLI or JSON proof.
// Even success supplies catalog evidence, never native execution admission.
func VerifyRegistered(id, expectedSHA string, expected Binding) (Evidence, error) {
	if !ValidID(id) || !validDigest(expectedSHA) {
		return Evidence{}, refusal("authority_reference_invalid")
	}
	policy := fsPolicy{0, "/"}
	name := filepath.Join(Registry, id, "authority.json")
	raw, err := readTrustedDocument(name, policy)
	if err != nil {
		return Evidence{}, err
	}
	if hashBytes(raw) != expectedSHA {
		return Evidence{}, refusal("authority_document_changed")
	}
	catalog, err := Parse(raw)
	if err != nil {
		return Evidence{}, err
	}
	if catalog.ID != id || catalog.Binding != expected || catalog.Binding.Platform != runtime.GOOS || catalog.Binding.Architecture != runtime.GOARCH {
		return Evidence{}, refusal("authority_binding_mismatch")
	}
	evidence, err := verifyFiles(catalog, policy)
	if err != nil {
		return Evidence{}, err
	}
	fresh, err := readTrustedDocument(name, policy)
	if err != nil || hashBytes(fresh) != expectedSHA {
		return Evidence{}, refusal("authority_document_changed")
	}
	evidence.AuthoritySHA256 = expectedSHA
	return evidence, nil
}

func hashBytes(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func trustedInfo(info os.FileInfo, policy fsPolicy, directory bool) error {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != policy.uid || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeDevice|os.ModeNamedPipe|os.ModeSocket) != 0 ||
		(info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0022 != 0) {
		return refusal("authority_path_untrusted")
	}
	if directory && !info.IsDir() {
		return refusal("authority_ancestor_not_directory")
	}
	return nil
}

func trustedAncestors(name string, policy fsPolicy) error {
	if !absolute(name) || !within(name, policy.boundary) && policy.boundary != "/" {
		return refusal("authority_path_outside_boundary")
	}
	parent := filepath.Dir(name)
	for {
		info, err := os.Lstat(parent)
		if err != nil {
			return refusal("authority_ancestor_unavailable")
		}
		if err = trustedInfo(info, policy, true); err != nil {
			return err
		}
		if err = noACL(parent); err != nil {
			return err
		}
		if parent == policy.boundary {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			return refusal("authority_path_outside_boundary")
		}
		parent = next
	}
	return nil
}

func openTrusted(name string, policy fsPolicy, maximum int64) (*os.File, os.FileInfo, error) {
	if err := trustedAncestors(name, policy); err != nil {
		return nil, nil, err
	}
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, refusal("authority_file_unavailable")
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, refusal("authority_file_unavailable")
	}
	if err = trustedInfo(info, policy, false); err != nil {
		file.Close()
		return nil, nil, err
	}
	s := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || s.Nlink != 1 || info.Size() < 0 || info.Size() > maximum {
		file.Close()
		return nil, nil, refusal("authority_file_unsafe")
	}
	if err = noACL(name); err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func readTrustedDocument(name string, policy fsPolicy) ([]byte, error) {
	file, before, err := openTrusted(name, policy, MaxDocument)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxDocument+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || int64(len(raw)) != before.Size() || !sameSnapshot(before, after) {
		return nil, refusal("authority_document_changed")
	}
	return raw, nil
}

func readAsset(value File, policy fsPolicy, maximum int64) (*os.File, os.FileInfo, error) {
	file, before, err := openTrusted(value.Path, policy, maximum)
	if err != nil {
		return nil, nil, err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || n != value.Size || n != before.Size() || hex.EncodeToString(hash.Sum(nil)) != value.SHA256 || !sameSnapshot(before, after) {
		file.Close()
		return nil, nil, refusal("authority_asset_changed")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, nil, refusal("authority_asset_unreadable")
	}
	return file, before, nil
}

func metadataMatches(name string, info os.FileInfo, expected Metadata, policy fsPolicy) error {
	if err := trustedInfo(info, policy, false); err != nil {
		return err
	}
	s := info.Sys().(*syscall.Stat_t)
	if uint32(info.Mode().Perm()) != expected.Mode || s.Gid != expected.GID || uint64(s.Dev) != expected.Device || uint64(s.Ino) != expected.Inode || info.ModTime().UnixNano() != expected.MtimeNS {
		return refusal("authority_metadata_changed")
	}
	attrs, err := attributes(name)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(attrs, expected.Xattrs) {
		return refusal("authority_xattrs_changed")
	}
	return noACL(name)
}

func attributes(name string) (map[string]string, error) {
	size, err := unix.Llistxattr(name, nil)
	if err != nil || size < 0 || size > 65536 {
		return nil, refusal("authority_metadata_unavailable")
	}
	result := map[string]string{}
	if size == 0 {
		return result, nil
	}
	buffer := make([]byte, size)
	got, err := unix.Llistxattr(name, buffer)
	if err != nil || got != size || buffer[size-1] != 0 {
		return nil, refusal("authority_metadata_changed")
	}
	for _, key := range strings.Split(string(buffer[:size-1]), "\x00") {
		if !cleanText(key) || strings.Contains(key, "posix_acl") {
			return nil, refusal("authority_metadata_unsupported")
		}
		if _, exists := result[key]; exists {
			return nil, refusal("authority_metadata_changed")
		}
		length, err := unix.Lgetxattr(name, key, nil)
		if err != nil || length < 0 || length > 65536 {
			return nil, refusal("authority_metadata_unavailable")
		}
		value := make([]byte, length)
		got, err := unix.Lgetxattr(name, key, value)
		if err != nil || got != length {
			return nil, refusal("authority_metadata_changed")
		}
		result[key] = hashBytes(value)
	}
	return result, nil
}

func verifyEntry(name string, entry Entry, policy fsPolicy) (os.FileInfo, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, refusal("authority_member_unavailable")
	}
	if err = metadataMatches(name, info, entry.Metadata, policy); err != nil {
		return nil, err
	}
	switch entry.Kind {
	case "directory":
		if !info.IsDir() {
			return nil, refusal("authority_member_kind_changed")
		}
	case "symlink":
		if info.Mode()&os.ModeSymlink == 0 {
			return nil, refusal("authority_member_kind_changed")
		}
		target, err := os.Readlink(name)
		if err != nil || target != *entry.Link {
			return nil, refusal("authority_link_changed")
		}
	case "file":
		file, before, err := readAsset(File{name, entry.File.SHA256, entry.File.Size}, policy, MaxFile)
		if err != nil {
			return nil, err
		}
		file.Close()
		if !sameSnapshot(info, before) {
			return nil, refusal("authority_member_changed")
		}
	}
	return info, nil
}

func treeNames(root *os.Root) ([]string, error) {
	names := []string{}
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return refusal("authority_inventory_unavailable")
		}
		if len(names) >= MaxMembers {
			return refusal("authority_member_count")
		}
		names = append(names, name)
		return nil
	})
	sort.Strings(names)
	return names, err
}

func verifySearch(value SearchPath, policy fsPolicy) error {
	if value.State == "absent" {
		current := value.Path
		for {
			_, err := os.Lstat(current)
			if err == nil {
				if current == value.Path {
					return refusal("authority_absence_changed")
				}
				return trustedAncestors(filepath.Join(current, "missing-child"), policy)
			}
			if !os.IsNotExist(err) {
				return refusal("authority_search_unavailable")
			}
			next := filepath.Dir(current)
			if next == current {
				return refusal("authority_search_unavailable")
			}
			current = next
		}
	}
	if err := trustedAncestors(filepath.Join(value.Path, "member"), policy); err != nil {
		return err
	}
	rows, err := os.ReadDir(value.Path)
	if err != nil {
		return refusal("authority_search_unavailable")
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name())
	}
	sort.Strings(names)
	expected := append([]string(nil), value.Members...)
	sort.Strings(expected)
	if !reflect.DeepEqual(names, expected) && !(len(names) == 0 && len(expected) == 0) {
		return refusal("authority_search_members_changed")
	}
	return nil
}

func verifyFiles(value Document, policy fsPolicy) (Evidence, error) {
	if err := value.Validate(); err != nil {
		return Evidence{}, err
	}
	checksums, _, err := readAsset(value.Publisher.Checksums, policy, MaxDocument)
	if err != nil {
		return Evidence{}, err
	}
	raw, err := io.ReadAll(checksums)
	checksums.Close()
	if err != nil {
		return Evidence{}, refusal("publisher_checksums_unreadable")
	}
	if err = checksumMatches(raw, value.Publisher); err != nil {
		return Evidence{}, err
	}
	archive, beforeArchive, err := readAsset(value.Publisher.Archive, policy, MaxArchive)
	if err != nil {
		return Evidence{}, err
	}
	members, err := archiveInventory(archive)
	afterArchive, statErr := archive.Stat()
	archive.Close()
	if err != nil {
		return Evidence{}, err
	}
	if statErr != nil || !sameSnapshot(beforeArchive, afterArchive) {
		return Evidence{}, refusal("publisher_archive_changed")
	}
	if err = compareArchive(value.Prefix.Entries, members); err != nil {
		return Evidence{}, err
	}
	if err = trustedAncestors(filepath.Join(value.Prefix.Root, "member"), policy); err != nil {
		return Evidence{}, err
	}
	root, err := os.OpenRoot(value.Prefix.Root)
	if err != nil {
		return Evidence{}, refusal("authority_prefix_unavailable")
	}
	defer root.Close()
	want := []string{}
	snapshots := map[string]os.FileInfo{}
	for _, entry := range value.Prefix.Entries {
		want = append(want, entry.Path)
		name := filepath.Join(value.Prefix.Root, entry.Path)
		info, err := verifyEntry(name, entry, policy)
		if err != nil {
			return Evidence{}, err
		}
		snapshots[name] = info
	}
	sort.Strings(want)
	names, err := treeNames(root)
	if err != nil {
		return Evidence{}, err
	}
	if !reflect.DeepEqual(want, names) {
		return Evidence{}, refusal("authority_prefix_member_set")
	}
	for _, external := range value.External {
		entry := external.Entry
		if err = trustedAncestors(entry.Path, policy); err != nil {
			return Evidence{}, err
		}
		info, err := verifyEntry(entry.Path, entry, policy)
		if err != nil {
			return Evidence{}, err
		}
		snapshots[entry.Path] = info
		if external.Asset != nil {
			asset, assetInfo, err := readAsset(*external.Asset, policy, MaxFile)
			if err != nil {
				return Evidence{}, err
			}
			asset.Close()
			if os.SameFile(info, assetInfo) {
				return Evidence{}, refusal("authority_asset_not_independent")
			}
			snapshots[external.Asset.Path] = assetInfo
		}
	}
	for _, search := range value.SearchPaths {
		if err = verifySearch(search, policy); err != nil {
			return Evidence{}, err
		}
	}
	if err = verifyNativeImages(value); err != nil {
		return Evidence{}, err
	}
	for name, before := range snapshots {
		after, err := os.Lstat(name)
		if err != nil || !sameSnapshot(before, after) {
			return Evidence{}, refusal("authority_member_changed")
		}
	}
	names, err = treeNames(root)
	if err != nil || !reflect.DeepEqual(want, names) {
		return Evidence{}, refusal("authority_prefix_member_set")
	}
	for _, search := range value.SearchPaths {
		if err = verifySearch(search, policy); err != nil {
			return Evidence{}, err
		}
	}
	return Evidence{Schema: "ags.interpreter-catalog-evidence.v1", AuthorityID: value.ID, Binding: value.Binding,
		Prefix: value.Prefix.Root, Executable: filepath.Join(value.Prefix.Root, value.Executable), PrefixMembers: len(value.Prefix.Entries), ExternalMembers: len(value.External), NativeImages: len(value.NativeImages),
		PublisherArchiveSHA256: value.Publisher.Archive.SHA256, CatalogVerified: true, NativeExecutionAllowed: false}, nil
}
