package gitstore

import (
	"context"
	"errors"
	"testing"
)

func TestUpdateRefCASWithHeadRejectsMovedRefs(t *testing.T) {
	for _, moved := range []string{"feature", "main"} {
		t.Run(moved, func(t *testing.T) {
			store, repo, _, base, head := checkedFixture(t)
			ctx := context.Background()
			next, err := store.WriteFile(ctx, repo, moved, "next.txt", "concurrent push", []byte("next\n"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateRefCASWithHead(ctx, repo, "refs/heads/feature", head, "refs/heads/main", head, base); !errors.Is(err, ErrMergePrecondition) {
				t.Fatalf("stale %s accepted: %v", moved, err)
			}
			wantBase, wantHead := base, head
			if moved == "main" {
				wantBase = next
			} else {
				wantHead = next
			}
			for ref, want := range map[string]string{"main": wantBase, "feature": wantHead} {
				if got, err := store.HeadSHA(ctx, repo, ref); err != nil || got != want {
					t.Fatalf("%s=%s want %s err=%v", ref, got, want, err)
				}
			}
		})
	}
}

func TestUpdateRefCASWithHeadVerifiesWithoutRewindingAdvancedBase(t *testing.T) {
	store, repo, _, base, head := checkedFixture(t)
	ctx := context.Background()
	if err := store.UpdateRefCASWithHead(ctx, repo, "refs/heads/feature", head, "refs/heads/main", head, base); err != nil {
		t.Fatal(err)
	}
	advanced, err := store.WriteFile(ctx, repo, "main", "next.txt", "advanced base", []byte("next\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRefCASWithHead(ctx, repo, "refs/heads/feature", head, "refs/heads/main", advanced, advanced); err != nil {
		t.Fatal(err)
	}
	if got, err := store.HeadSHA(ctx, repo, "main"); err != nil || got != advanced {
		t.Fatalf("main=%s want %s err=%v", got, advanced, err)
	}
	if _, err := store.WriteFile(ctx, repo, "feature", "moved.txt", "moved source", []byte("moved\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRefCASWithHead(ctx, repo, "refs/heads/feature", head, "refs/heads/main", advanced, advanced); !errors.Is(err, ErrMergePrecondition) {
		t.Fatalf("verification-only base accepted moved source: %v", err)
	}
	if got, err := store.HeadSHA(ctx, repo, "main"); err != nil || got != advanced {
		t.Fatalf("main=%s want %s err=%v", got, advanced, err)
	}
}
