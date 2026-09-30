package gitstore

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// HistoricalReadRevision resolves the explicit representation of an exact old
// commit only for content/history reads. Callers MUST NOT use it for approvals,
// CI identity, compare-and-swap, effect authorization, or live branch writes.
// Those continue to compare the original literal identities.
func (s *Store) HistoricalReadRevision(ctx context.Context, fullName, rev string) (string, error) {
	dir, err := s.repoPath(ctx, fullName)
	if err != nil {
		return "", err
	}
	return historicalReadRevision(ctx, dir, rev)
}

func historicalReadRevision(ctx context.Context, dir, rev string) (string, error) {
	oid := strings.ToLower(rev)
	if !canonicalMaintenanceOID(oid) {
		return rev, nil
	}
	ref := "refs/replace/" + oid
	cmd := exec.CommandContext(ctx, "git", "--no-replace-objects", "-C", dir, "for-each-ref", "--format=%(refname) %(objectname)", ref)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("historical representation lookup failed: %w", err)
	}
	text := strings.TrimSpace(string(output))
	if text == "" {
		return rev, nil
	}
	parts := strings.Fields(text)
	if len(parts) != 2 || parts[0] != ref || !canonicalMaintenanceOID(parts[1]) {
		return "", fmt.Errorf("invalid historical representation")
	}
	return parts[1], nil
}

// IsHistoricalAncestor checks preservation of already-recorded merge content.
// This read-only diagnostic may follow a recorded retirement representation;
// it must never be used to authorize a new merge or provider effect.
func (s *Store) IsHistoricalAncestor(ctx context.Context, fullName, older, newer string) (bool, error) {
	dir, err := s.repoPath(ctx, fullName)
	if err != nil {
		return false, err
	}
	if err := historicalReadRange(ctx, dir, &older, &newer); err != nil {
		return false, err
	}
	return s.IsAncestor(ctx, fullName, older, newer)
}

func historicalReadRange(ctx context.Context, dir string, revisions ...*string) error {
	for _, revision := range revisions {
		mapped, err := historicalReadRevision(ctx, dir, *revision)
		if err != nil {
			return err
		}
		*revision = mapped
	}
	return nil
}
