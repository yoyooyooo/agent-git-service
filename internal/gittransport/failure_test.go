package gittransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTransportFailureTaxonomy(t *testing.T) {
	for _, tc := range []struct{ text, category, reason string }{
		{"updates were rejected: pre-receive hook declined", "hook_rejected", "hook_declined"},
		{"updates were rejected: protected branch; pre-receive hook declined", "protection_policy", "protected_branch"},
		{"updates were rejected: HTTP 403 forbidden; pre-receive hook declined", "http_forbidden", "http_403_pattern"},
		{"cannot lock ref: is at X but expected Y; protected branch", "cas_conflict", "expected_old_mismatch"},
		{"cannot lock ref: reference already exists", "cas_conflict", "expected_absent_mismatch"},
		{"fatal: Unable to create '/fixture/ref.lock': File exists", "ref_lock_contention", "ref_lock_exists"},
		{"non-fast-forward", "non_fast_forward", "non_fast_forward"},
		{"Authentication failed", "auth_failed", "authentication_failed"},
		{"repository not found", "target_missing_or_inaccessible", "repository_not_found"},
		{"could not resolve host", "network", "dns_failed"},
		{"connection refused", "network", "connect_failed"},
		{"connection reset", "network", "connection_reset"},
		{"SSL certificate problem", "network", "tls_failed"},
		{"TLS handshake failed", "network", "tls_failed"},
		{"connection timed out", "network", "timeout"},
		{"\u202e\x00\x1b unicode hostile output", "unknown", "response_withheld"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			err := SafeError(errors.New(tc.text + " synthetic-secret"))
			got := Describe(fmt.Errorf("safe wrapper: %w", err))
			if got.Category != tc.category || got.ReasonCode != tc.reason || got.HTTPStatus != nil {
				t.Fatalf("wrong evidence: %+v", got)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(err.Error()+string(b), "synthetic-secret") {
				t.Fatal("response escaped")
			}
		})
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := fmt.Errorf("outer: %w", SafeError(fmt.Errorf("inner: %w", cause)))
		if !errors.Is(err, cause) || Describe(err).EvidenceSource != "context" {
			t.Fatal("lost context identity")
		}
	}
}
func TestTransportFailureTruncationAndSplitPatterns(t *testing.T) {
	var out limitedOutput
	out.Write([]byte(strings.Repeat("x", outputLimit+10)))
	out.Write([]byte("\nupdates rejected; protected bra"))
	out.Write([]byte("nch; synthetic-token\n"))
	f := out.scan.Failure()
	if !out.overflow || len(out.data) != outputLimit || f.Category != "protection_policy" {
		t.Fatalf("lost evidence after cap: %+v", f)
	}
}
func TestTransportTypedProcessFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	// stderr contains all output classes of secret, including non-URL secrets.
	script := "#!/bin/sh\nprintf '%s\\n' 'Authorization: sentinel-header /private/sentinel-path sentinel-body https://user:sentinel-token@example.test/repo pre-receive hook declined' >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	_, err := Run(context.Background(), "https://provider.example.test/repo", "", "push", "https://provider.example.test/repo", "main:main")
	var typed *TransportError
	if !errors.As(err, &typed) {
		t.Fatalf("not typed: %T", err)
	}
	f := typed.Failure()
	if f.Category != "hook_rejected" || f.ExitCode == nil || *f.ExitCode != 128 {
		t.Fatalf("wrong failure: %+v", f)
	}
	b, _ := json.Marshal(f)
	for _, sentinel := range []string{"sentinel-header", "sentinel-path", "sentinel-body", "sentinel-token"} {
		if strings.Contains(err.Error()+string(b), sentinel) {
			t.Fatal("secret escaped")
		}
	}
}

func TestTransportProcessReportsTruncatedStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	// Native pipe chunks hit the output cap before the decisive rejection. The
	// scanner must still classify the tail without returning any raw bytes.
	script := "#!/bin/sh\n/bin/dd if=/dev/zero bs=65536 count=65 >&2 2>/dev/null\nprintf '%s\\n' 'protected branch sentinel-secret' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	remote := "https://provider.example.test/repo"
	_, err := Run(context.Background(), remote, "", "push", remote, "main:main")
	var typed *TransportError
	if !errors.As(err, &typed) || !typed.Failure().StderrTruncated || typed.Failure().Category != "protection_policy" || strings.Contains(err.Error(), "sentinel-secret") {
		t.Fatalf("lost truncation/classification: %v", err)
	}
}
