package forgejointegration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestClassifyProjectionErrorDetectsNonFastForward(t *testing.T) {
	err := errors.New("git push: exit status 1: To http://forgejo/example-owner/demo.git\n ! [rejected]        main -> main (non-fast-forward)\nerror: failed to push some refs")
	classified := ClassifyProjectionError("refs/heads/main", "abc123", err)

	if classified.Type != ProjectionFailureNonFastForward {
		t.Fatalf("Type=%q, want %q", classified.Type, ProjectionFailureNonFastForward)
	}
	if classified.Ref != "refs/heads/main" || classified.ExpectedSHA != "abc123" {
		t.Fatalf("unexpected classified error: %#v", classified)
	}
	if strings.Contains(classified.ErrorSummary, "http://forgejo") {
		t.Fatalf("classified error leaked remote URL: %q", classified.ErrorSummary)
	}
}

func TestClassifyProjectionErrorDetectsExistingRefLockConflict(t *testing.T) {
	err := errors.New("git push: signal: killed\nremote: error: cannot lock ref 'refs/heads/agent/large-binary': reference already exists")
	classified := ClassifyProjectionError("refs/heads/agent/large-binary", "b487eae1a5ecb7a6578ba449c6ec7ea19b9cf0bf", err)

	if classified.Type != ProjectionFailureNonFastForward {
		t.Fatalf("Type=%q, want %q", classified.Type, ProjectionFailureNonFastForward)
	}
	if !strings.Contains(classified.ErrorSummary, "git push: signal: killed") || !strings.Contains(classified.ErrorSummary, "reference already exists") {
		t.Fatalf("classified error lost retry evidence: %q", classified.ErrorSummary)
	}
}

func TestClassifyProjectionErrorDetectsProtectedBranch(t *testing.T) {
	err := errors.New("git push: exit status 1: remote: Forgejo: Not allowed to push to protected branch master\n ! [remote rejected] master -> master (pre-receive hook declined)")
	classified := ClassifyProjectionError("refs/heads/master", "abc123", err)

	if classified.Type != ProjectionFailureProtectedBranch {
		t.Fatalf("Type=%q, want %q", classified.Type, ProjectionFailureProtectedBranch)
	}
	if classified.Ref != "refs/heads/master" || classified.ExpectedSHA != "abc123" {
		t.Fatalf("unexpected classified error: %#v", classified)
	}
}

func TestHandlePushWrapsMirrorFailureWithProjectionError(t *testing.T) {
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		MirrorBranchIncludes: []string{"main"},
	}, &fakeClient{}, func(ctx context.Context, req PushRequest) error {
		return errors.New("git push: exit status 1: ! [rejected] main -> main (non-fast-forward)")
	})

	err := integration.HandlePush(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Changes: []RefChange{{
			Ref:   "refs/heads/main",
			After: "abc123",
		}},
	})
	var projectionErr *ProjectionError
	if !errors.As(err, &projectionErr) {
		t.Fatalf("error %T %[1]v does not wrap ProjectionError", err)
	}
	if projectionErr.Type != ProjectionFailureNonFastForward || projectionErr.TargetRepo != "example-owner/demo" {
		t.Fatalf("unexpected projection error: %#v", projectionErr)
	}
}

func TestSanitizeProjectionErrorSummaryRedactsCredentialsForAnyURLScheme(t *testing.T) {
	input := `push "file://x-access-token:secret@/tmp/forgejo/repo.git" failed; mirror ssh://bot:password@forgejo.example/repo.git`
	got := SanitizeProjectionErrorSummary(input)
	for _, forbidden := range []string{"x-access-token", "secret", "password", "/tmp/forgejo"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitized summary leaked %q: %s", forbidden, got)
		}
	}
	if strings.Count(got, "<redacted>") != 2 {
		t.Fatalf("expected both credential URLs redacted: %s", got)
	}
}
