package interpreterauthority

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// RegisteredCatalog is created only by reading the fixed root registry. Its
// document is private so a caller cannot replace a verified member in memory.
// The handle remains a file authority, not process or execution admission.
type RegisteredCatalog struct {
	id       string
	digest   string
	binding  Binding
	document Document
	evidence Evidence
}

func OpenRegistered(id, expectedSHA string, expected Binding) (*RegisteredCatalog, error) {
	if !ValidID(id) || !validDigest(expectedSHA) {
		return nil, refusal("authority_reference_invalid")
	}
	name := filepath.Join(Registry, id, "authority.json")
	raw, err := ReadProtected(name, MaxDocument)
	if err != nil {
		return nil, err
	}
	if hashBytes(raw) != expectedSHA {
		return nil, refusal("authority_document_changed")
	}
	doc, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if doc.ID != id || doc.Binding != expected || expected.Platform != runtime.GOOS || expected.Architecture != runtime.GOARCH {
		return nil, refusal("authority_binding_mismatch")
	}
	ev, err := verifyFiles(doc, fsPolicy{0, "/"})
	if err != nil {
		return nil, err
	}
	fresh, err := ReadProtected(name, MaxDocument)
	if err != nil || hashBytes(fresh) != expectedSHA {
		return nil, refusal("authority_document_changed")
	}
	ev.AuthoritySHA256 = expectedSHA
	return &RegisteredCatalog{id, expectedSHA, expected, doc, ev}, nil
}

func (r *RegisteredCatalog) Evidence() Evidence { return r.evidence }

func (r *RegisteredCatalog) Recheck() error {
	_, err := OpenRegistered(r.id, r.digest, r.binding)
	return err
}

// FileIdentity is used by a native parent to compare kernel-observed file
// mappings. It does not authorize an unverified process or unknown mapping.
type FileIdentity struct {
	Path   string
	SHA256 string
	Device uint64
	Inode  uint64
}

func (r *RegisteredCatalog) Files() map[string]FileIdentity {
	out := map[string]FileIdentity{}
	for _, e := range r.document.Prefix.Entries {
		if e.Kind == "file" {
			p := filepath.Join(r.document.Prefix.Root, e.Path)
			out[p] = FileIdentity{p, e.File.SHA256, e.Metadata.Device, e.Metadata.Inode}
		}
	}
	for _, external := range r.document.External {
		e := external.Entry
		if e.Kind == "file" {
			out[e.Path] = FileIdentity{e.Path, e.File.SHA256, e.Metadata.Device, e.Metadata.Inode}
		}
	}
	return out
}

// ReadProtected provides the same bounded, no-follow root-owned read used by
// the catalog. It performs no interpretation, execution or mutation.
func ReadProtected(name string, maximum int64) ([]byte, error) {
	if maximum <= 0 || maximum > MaxFile {
		return nil, refusal("authority_read_limit")
	}
	f, before, err := openTrusted(name, fsPolicy{0, "/"}, maximum)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maximum+1))
	after, statErr := f.Stat()
	if err != nil || statErr != nil || len(raw) > int(maximum) || int64(len(raw)) != before.Size() || !sameSnapshot(before, after) {
		return nil, refusal("authority_document_changed")
	}
	return raw, nil
}

func ReadProtectedFile(reference File, maximum int64) ([]byte, error) {
	if err := reference.validate(maximum); err != nil {
		return nil, err
	}
	raw, err := ReadProtected(reference.Path, maximum)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != reference.Size || hashBytes(raw) != reference.SHA256 {
		return nil, refusal("authority_asset_changed")
	}
	return raw, nil
}

// ProtectedDirectory checks the named directory as well as all ancestors.
// A native parent may additionally demand a private final mode for its state.
func ProtectedDirectory(name string, private bool) error {
	if err := trustedAncestors(filepath.Join(name, "member"), fsPolicy{0, "/"}); err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil || (private && info.Mode().Perm() != 0700) {
		return refusal("authority_directory_invalid")
	}
	return nil
}
