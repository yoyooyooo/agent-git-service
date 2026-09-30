package artifactretirement

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func validateLocalObjectStore(source string) error {
	objects := filepath.Join(source, "objects")
	return filepath.WalkDir(objects, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("retirement refuses symlinked object storage")
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("retirement requires regular object files")
		}
		if !entry.IsDir() && (entry.Name() == "alternates" || entry.Name() == "http-alternates" || strings.HasSuffix(entry.Name(), ".promisor")) {
			return errors.New("retirement requires a complete local object store")
		}
		return nil
	})
}

func refUpdatesBetween(original, clean map[string]string) []RefUpdate {
	names := map[string]bool{}
	for name := range original {
		names[name] = true
	}
	for name := range clean {
		names[name] = true
	}
	updates := make([]RefUpdate, 0)
	for name := range names {
		if original[name] != clean[name] {
			updates = append(updates, RefUpdate{Ref: name, Old: original[name], New: clean[name]})
		}
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Ref < updates[j].Ref })
	return updates
}

func restoreStagingRefs(ctx context.Context, stage gitRunner, sourceRefs map[string]string) error {
	current, err := refMap(ctx, stage, true)
	if err != nil {
		return err
	}
	updates := refUpdatesBetween(current, sourceRefs)
	if len(updates) == 0 {
		return nil
	}
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, update := range updates {
		switch {
		case update.Old == "":
			fmt.Fprintf(&tx, "create %s %s\n", update.Ref, update.New)
		case update.New == "":
			fmt.Fprintf(&tx, "delete %s %s\n", update.Ref, update.Old)
		default:
			fmt.Fprintf(&tx, "update %s %s %s\n", update.Ref, update.New, update.Old)
		}
	}
	tx.WriteString("prepare\ncommit\n")
	_, err = stage.run(ctx, []byte(tx.String()), "update-ref", "--stdin")
	return err
}

func rootObjectTypes(ctx context.Context, g gitRunner, roots []string) (map[string]string, error) {
	result := map[string]string{}
	if len(roots) == 0 {
		return result, nil
	}
	out, err := g.run(ctx, []byte(strings.Join(roots, "\n")+"\n"), "--no-replace-objects", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(roots) {
		return nil, errors.New("retirement root inventory incomplete")
	}
	for i, line := range lines {
		parts := strings.Fields(line)
		if len(parts) != 2 || parts[0] != roots[i] {
			return nil, errors.New("retirement root inventory invalid")
		}
		switch parts[1] {
		case "missing", "blob", "tree", "commit", "tag":
		default:
			return nil, errors.New("retirement root type invalid")
		}
		result[parts[0]] = parts[1]
	}
	return result, nil
}

func protectStagingApplicationRoots(ctx context.Context, stage, source gitRunner, roots []string) error {
	if len(roots) > 100000 {
		return errors.New("retirement application root budget exceeded")
	}
	set := map[string]bool{}
	for _, oid := range roots {
		if !fullOID(oid) || oid == strings.Repeat("0", 40) {
			return errors.New("invalid retirement application root")
		}
		set[oid] = true
	}
	ordered := make([]string, 0, len(set))
	for oid := range set {
		ordered = append(ordered, oid)
	}
	sort.Strings(ordered)
	original, err := rootObjectTypes(ctx, source, ordered)
	if err != nil {
		return err
	}
	copied, err := rootObjectTypes(ctx, stage, ordered)
	if err != nil {
		return err
	}
	refs, err := refMap(ctx, stage, true)
	if err != nil {
		return err
	}
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, oid := range ordered {
		if original[oid] == "missing" {
			continue
		} // Existing absence is not repaired or fabricated.
		if copied[oid] != original[oid] {
			return errors.New("available application object was omitted from retirement staging")
		}
		ref := "refs/ags/retention/" + oid
		if current, ok := refs[ref]; ok {
			if current != oid {
				return errors.New("retirement application retention ref is inconsistent")
			}
			fmt.Fprintf(&tx, "verify %s %s\n", ref, oid)
		} else {
			fmt.Fprintf(&tx, "create %s %s\n", ref, oid)
		}
	}
	tx.WriteString("prepare\ncommit\n")
	_, err = stage.run(ctx, []byte(tx.String()), "update-ref", "--stdin")
	return err
}
