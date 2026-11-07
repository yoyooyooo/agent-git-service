package gitstore_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/gitstore"
)

func TestStoreInitNativeGitBareRepository(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("native git is required")
	}

	for _, tc := range []struct {
		name     string
		fullName string
		seed     bool
	}{
		{name: "empty", fullName: "owner/repository"},
		{name: "seeded", fullName: "owner/repository", seed: true},
		{name: "wiki", fullName: "owner/repository.wiki"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, cleanup := gitstore.NewTestStore(t)
			defer cleanup()
			ctx := context.Background()
			if err := store.Init(ctx, tc.fullName, "main", tc.seed); err != nil {
				t.Fatal(err)
			}
			repoPath, err := store.GetRepoPath(ctx, tc.fullName)
			if err != nil {
				t.Fatal(err)
			}

			// Explicit --git-dir callers must see a bare repository without
			// relying on discovery from the repository's working directory.
			outside := t.TempDir()
			checkGit := func(stage, want string, args ...string) {
				t.Helper()
				cmd := exec.Command(gitPath, append([]string{"--git-dir=" + repoPath}, args...)...)
				cmd.Dir = outside
				cmd.Env = []string{
					"PATH=" + os.Getenv("PATH"),
					"HOME=" + outside,
					"GIT_CONFIG_NOSYSTEM=1",
					"GIT_CONFIG_GLOBAL=" + os.DevNull,
					"GIT_CONFIG_COUNT=0",
					"GIT_OPTIONAL_LOCKS=0",
					"GIT_TERMINAL_PROMPT=0",
				}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Errorf("%s: git %v: %v: %s", stage, args, err, out)
					return
				}
				if got := strings.TrimSpace(string(out)); got != want {
					t.Errorf("%s: git %v = %q, want %q", stage, args, got, want)
				}
			}
			checkGit("after init", "true", "rev-parse", "--is-bare-repository")
			checkGit("after init", "0", "config", "--local", "--get", "core.repositoryformatversion")

			const baseURL = "https://git.example.test"
			if err := store.SetupConfig(ctx, tc.fullName, baseURL); err != nil {
				t.Fatal(err)
			}
			checkGit("after SetupConfig", "true", "rev-parse", "--is-bare-repository")
			checkGit("after SetupConfig", "0", "config", "--local", "--get", "core.repositoryformatversion")
			checkGit("after SetupConfig", baseURL+"/"+tc.fullName+".git", "config", "--local", "--get", "remote.origin.url")
		})
	}
}
