package forgejointegration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
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
	if classified.Diagnostic == nil || classified.Diagnostic.Category != "cas_conflict" || classified.Diagnostic.ReasonCode != "expected_absent_mismatch" {
		t.Fatalf("classified error lost typed CAS evidence: %+v", classified.Diagnostic)
	}
	if strings.Contains(classified.ErrorSummary, "signal: killed") {
		t.Fatal("raw legacy response escaped safe summary")
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

func TestTruncateProjectionErrorUTF8(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{name: "ASCII within budget", input: "short", max: 10, want: "short"},
		{name: "ASCII truncated", input: "abcdefghijk", max: 10, want: "abcdefg..."},
		{name: "ASCII exact budget", input: "abcdefghij", max: 10, want: "abcdefghij"},
		{name: "Chinese exact budget", input: "中文", max: 6, want: "中文"},
		{name: "Chinese crosses boundary", input: "中文中文", max: 10, want: "中文..."},
		{name: "Chinese fills prefix budget", input: "中文中文", max: 9, want: "中文..."},
		{name: "production byte limit", input: strings.Repeat("中", 400), max: 1000, want: strings.Repeat("中", 332) + "..."},
		{name: "emoji crosses boundary", input: "a🙂🙂", max: 7, want: "a..."},
		{name: "emoji fills prefix budget", input: "a🙂🙂", max: 8, want: "a🙂..."},
		{name: "ASCII one byte", input: "abcd", max: 1, want: "a"},
		{name: "ASCII two bytes", input: "abcd", max: 2, want: "ab"},
		{name: "ASCII three bytes", input: "abcd", max: 3, want: "abc"},
		{name: "Chinese too small", input: "中文", max: 2, want: ""},
		{name: "Chinese three bytes", input: "中文", max: 3, want: "中"},
		{name: "emoji too small", input: "🙂", max: 3, want: ""},
		{name: "only ellipsis fits", input: "🙂🙂", max: 4, want: "..."},
		{name: "zero budget", input: "abc", max: 0, want: ""},
		{name: "negative budget", input: "abc", max: -1, want: ""},
		{name: "empty", input: "", max: 10, want: ""},
		{name: "empty zero budget", input: "", max: 0, want: ""},
		{name: "empty negative budget", input: "", max: -1, want: ""},
		// Each consecutive invalid byte sequence becomes one U+FFFD before
		// measuring the byte budget, including when the original bytes fit.
		{name: "invalid within budget", input: "a\xffb", max: 10, want: "a\ufffdb"},
		{name: "invalid sequence", input: "a\xff\xfeb", max: 10, want: "a\ufffdb"},
		{name: "incomplete rune", input: "a\xe4\xb8", max: 10, want: "a\ufffd"},
		{name: "replacement expands beyond budget", input: "ab\xffcd", max: 5, want: "ab..."},
		{name: "replacement exact budget", input: "a\xffb", max: 5, want: "a\ufffdb"},
		{name: "replacement too small", input: "\xff", max: 2, want: ""},
		{name: "replacement three bytes", input: "\xff", max: 3, want: "\ufffd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Keep later cases running if a boundary case panics on the old code.
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("truncateProjectionError panicked: %v", recovered)
				}
			}()
			got := truncateProjectionError(tt.input, tt.max)
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8 summary: %q (%d bytes)", got, len(got))
			}
			if (tt.max >= 0 && len(got) > tt.max) || (tt.max < 0 && got != "") {
				t.Errorf("summary has %d bytes, budget %d", len(got), tt.max)
			}
			if got != tt.want {
				t.Errorf("summary=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeProjectionErrorSummaryUTF8AndBudget(t *testing.T) {
	for _, prefix := range []string{"", strings.Repeat("中", 330)} {
		t.Run(fmt.Sprintf("prefix_bytes_%d", len(prefix)), func(t *testing.T) {
			// Redaction shortens a URL that crosses the truncation boundary.
			// Its replacement must be included before applying the byte limit.
			url := "https://fixture-user:fixture-password@example.invalid/" + strings.Repeat("x", 1200)
			tail := " failed " + strings.Repeat("🙂", 300) + "\xff"
			got := SanitizeProjectionErrorSummary(prefix + url + tail)
			want := truncateProjectionError(prefix+"https://<redacted>"+tail, 1000)
			if got != want {
				t.Fatalf("summary=%q, want redaction before truncation %q", got, want)
			}
			if !utf8.ValidString(got) || len(got) > 1000 {
				t.Fatalf("summary must be valid UTF-8 within 1000 bytes: valid=%t bytes=%d", utf8.ValidString(got), len(got))
			}
			for _, forbidden := range []string{"fixture-user", "fixture-password", "example.invalid", strings.Repeat("x", 10)} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("summary leaked synthetic URL component %q", forbidden)
				}
			}
		})
	}
	t.Run("redaction expands beyond budget", func(t *testing.T) {
		prefix := strings.Repeat("a", 990)
		got := SanitizeProjectionErrorSummary(prefix + "a://x")
		if want := prefix + "a://<re..."; got != want || len(got) > 1000 {
			t.Fatalf("expanded redaction summary=%q (%d bytes), want %q", got, len(got), want)
		}
	})
	got := SanitizeProjectionErrorSummary("failure \xff\xfe detail")
	if want := "failure \ufffd detail"; got != want || !utf8.ValidString(got) {
		t.Fatalf("short invalid input summary=%q, want %q", got, want)
	}
}

func TestClassifyProjectionErrorDetectsSanitizedForbidden(t *testing.T) {
	got := ClassifyProjectionError("refs/heads/main", "abc123", errors.New("provider Git 403 forbidden: exit status 128"))
	if got.Type != ProjectionFailurePermissionDenied {
		t.Fatalf("unclassified HTTP rejection: %s", got.Type)
	}
}
