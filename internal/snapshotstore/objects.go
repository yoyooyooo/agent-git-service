package snapshotstore

import (
	"context"
	"errors"
	"strings"
)

// ContainsObjects checks only this pinned self-contained view, using isolated
// native Git. It is a selection hint, not an access grant or a change to native
// upload-pack's want policy. Callers must retain the lease throughout use.
func (v *Lease) ContainsObjects(ctx context.Context, oids []string) (bool, error) {
	if len(oids) == 0 {
		return true, nil
	}
	if len(oids) > 4096 {
		return false, errors.New("too many requested objects")
	}
	length := 40
	if v.Snapshot().ObjectFormat == "sha256" {
		length = 64
	}
	for _, oid := range oids {
		if len(oid) != length || strings.Trim(oid, "0") == "" || strings.Trim(oid, "0123456789abcdef") != "" {
			return false, errors.New("invalid requested object")
		}
	}
	output, err := runGit(ctx, v.RepoPath(), strings.NewReader(strings.Join(oids, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return false, err
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != len(oids) {
		return false, errors.New("invalid object lookup result")
	}
	for i, line := range lines {
		fields := strings.Split(line, " ")
		if len(fields) != 2 || fields[0] != oids[i] {
			return false, errors.New("invalid object lookup binding")
		}
		switch fields[1] {
		case "missing":
			return false, nil
		case "commit", "tag", "tree", "blob":
		default:
			return false, errors.New("invalid object lookup type")
		}
	}
	return true, nil
}
