package gitstore

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// Only storage roots are translated. Historical DB facts stay on their original
// identity, while their explicitly retained cleaned representation remains a
// physical GC root. A pre-existing missing object without an alias stays missing.
func historicalRetentionRoots(ctx context.Context, dir string, roots []string) ([]string, error) {
	out, err := maintenanceGit(ctx, dir, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/replace/")
	if err != nil {
		return nil, err
	}
	aliases := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || !strings.HasPrefix(parts[0], "refs/replace/") || !canonicalMaintenanceOID(parts[1]) {
			return nil, errors.New("invalid historical retention representation")
		}
		original := strings.TrimPrefix(parts[0], "refs/replace/")
		if !canonicalMaintenanceOID(original) {
			return nil, errors.New("invalid historical retention identity")
		}
		aliases[original] = parts[1]
	}
	set := map[string]bool{}
	for _, oid := range roots {
		if mapped, ok := aliases[oid]; ok {
			oid = mapped
		}
		set[oid] = true
	}
	result := make([]string, 0, len(set))
	for oid := range set {
		result = append(result, oid)
	}
	sort.Strings(result)
	return result, nil
}
