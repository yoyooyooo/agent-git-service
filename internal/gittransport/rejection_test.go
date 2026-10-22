package gittransport

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProviderRejectionsRetainOnlySafeCategory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX subprocess fixture")
	}
	for _, tc := range []struct{ response, category string }{
		{"remote: Forgejo: User is not allowed to push to protected branch main (pre-receive hook declined)", "protected branch rejected"},
		{"fatal: unable to access remote: The requested URL returned error: 403", "403 forbidden"},
		{"remote: unrelated failure", "operation failed (response withheld)"},
	} {
		t.Run(tc.category, func(t *testing.T) {
			dir := t.TempDir()
			const secret = "fixture-private-token"
			script := "#!/bin/sh\nprintf '%s\\n' '" + tc.response + " " + secret + "' >&2\nexit 1\n"
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			remote := "https://provider.example.test/repository.git"
			_, err := Run(context.Background(), remote, secret, "push", remote, "main:main")
			if err == nil || !strings.Contains(err.Error(), tc.category) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "unable to access") {
				t.Fatalf("unsafe or unclassified rejection: %v", err)
			}
		})
	}
}
