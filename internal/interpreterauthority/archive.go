package interpreterauthority

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path"
	"strings"
)

type archiveMember struct {
	kind string
	mode uint32
	size int64
	sha  string
	link string
}

// tar.Reader consumes extension records internally. Bound every decompressed
// byte before it reaches tar, including hidden PAX/GNU headers and padding.
type archiveBudget struct {
	reader    io.Reader
	remaining int64
}

func (b *archiveBudget) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, refusal("publisher_archive_stream_limit")
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.reader.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func checksumMatches(raw []byte, publisher Publisher) error {
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == publisher.AssetName {
			if fields[0] != publisher.Archive.SHA256 {
				return refusal("publisher_checksum_mismatch")
			}
			count++
		}
	}
	if count != 1 {
		return refusal("publisher_checksum_missing_or_duplicate")
	}
	return nil
}

// archiveInventory derives members from the publisher bytes; the document
// cannot shrink this inventory. It never extracts or executes an archive.
func archiveInventory(reader io.Reader) (map[string]archiveMember, error) {
	input := bufio.NewReader(reader)
	gz, err := gzip.NewReader(input)
	if err != nil {
		return nil, refusal("publisher_archive_invalid")
	}
	defer gz.Close()
	gz.Multistream(false)
	stream := &archiveBudget{gz, MaxExpanded + (64 << 20)}
	tr := tar.NewReader(stream)
	entries := map[string]archiveMember{".": {kind: "directory", mode: 0755}}
	seen := map[string]bool{}
	var expanded int64
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, refusal("publisher_archive_invalid")
		}
		if count >= MaxMembers || h.Size < 0 || h.Size > MaxFile || h.Mode&^0777 != 0 || h.Uid != 0 || h.Gid != 0 || len(h.Xattrs) != 0 {
			return nil, refusal("publisher_member_unsupported")
		}
		for key := range h.PAXRecords {
			if key != "path" && key != "linkpath" && key != "mtime" && key != "size" {
				return nil, refusal("publisher_member_unsupported")
			}
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name != "python" && !strings.HasPrefix(name, "python/") {
			return nil, refusal("publisher_member_path")
		}
		name = strings.TrimPrefix(name, "python/")
		if name == "python" {
			name = "."
		}
		if (name != "." && !relative(name)) || seen[name] {
			return nil, refusal("publisher_member_path")
		}
		seen[name] = true
		member := archiveMember{}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			member.kind, member.mode, member.size = "file", 0644, h.Size
			if h.Mode&0111 != 0 {
				member.mode = 0755
			}
			expanded += h.Size
			if expanded > MaxExpanded {
				return nil, refusal("publisher_expanded_size")
			}
			hash := sha256.New()
			n, err := io.Copy(hash, io.LimitReader(tr, h.Size+1))
			if err != nil || n != h.Size {
				return nil, refusal("publisher_member_size")
			}
			member.sha = hex.EncodeToString(hash.Sum(nil))
		case tar.TypeDir:
			if h.Size != 0 {
				return nil, refusal("publisher_member_size")
			}
			member.kind, member.mode = "directory", 0755
		case tar.TypeSymlink:
			if h.Size != 0 || !cleanText(h.Linkname) || path.IsAbs(h.Linkname) {
				return nil, refusal("publisher_link_invalid")
			}
			member.kind, member.mode, member.link = "symlink", 0777, h.Linkname
		default:
			return nil, refusal("publisher_member_unsupported")
		}
		if prior, found := entries[name]; found && (prior.kind != "directory" || member.kind != "directory") {
			return nil, refusal("publisher_member_collision")
		}
		entries[name] = member
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if prior, found := entries[parent]; found && prior.kind != "directory" {
				return nil, refusal("publisher_parent_invalid")
			}
			entries[parent] = archiveMember{kind: "directory", mode: 0755}
		}
		if len(entries) > MaxMembers {
			return nil, refusal("publisher_member_count")
		}
	}
	// Finish the gzip stream to validate its CRC. Only bounded zero padding is
	// accepted after tar EOF; concatenated gzip/tar streams are not hidden inputs.
	tail, err := io.ReadAll(io.LimitReader(stream, 10241))
	if err != nil || len(tail) > 10240 || !bytes.Equal(tail, make([]byte, len(tail))) {
		return nil, refusal("publisher_archive_trailing")
	}
	if _, err := input.Peek(1); err != io.EOF {
		return nil, refusal("publisher_archive_trailing")
	}
	return entries, nil
}

func compareArchive(entries []Entry, archive map[string]archiveMember) error {
	if len(entries) != len(archive) {
		return refusal("publisher_prefix_member_set")
	}
	for _, entry := range entries {
		member, found := archive[entry.Path]
		if !found || member.kind != entry.Kind || member.mode != entry.Metadata.Mode {
			return refusal("publisher_prefix_member_mismatch")
		}
		if entry.File != nil && (entry.File.Size != member.size || entry.File.SHA256 != member.sha) {
			return refusal("publisher_prefix_content_mismatch")
		}
		if entry.Link != nil && *entry.Link != member.link {
			return refusal("publisher_prefix_link_mismatch")
		}
	}
	return nil
}
