package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSupportedBuildsInjectVerifiedExactSourceRevision(t *testing.T) {
	root := filepath.Clean("..")
	makefile := readBuildContractFile(t, filepath.Join(root, "Makefile"))
	for _, required := range []string{
		"GIT_SHA    ?= $(shell git rev-parse --verify HEAD",
		"./scripts/build-exact-source.sh verify",
		"./scripts/build-exact-source.sh binary",
		"./scripts/build-exact-source.sh docker",
	} {
		if !strings.Contains(makefile, required) {
			t.Fatalf("Makefile source-revision contract missing %q", required)
		}
	}
	dockerfile := readBuildContractFile(t, filepath.Join(root, "Dockerfile"))
	for _, required := range []string{"ARG GIT_SHA", "ARG GIT_TREE", ".ags-source-revision", ".ags-source-tree", "server.gitSHA=${GIT_SHA}"} {
		if !strings.Contains(dockerfile, required) {
			t.Fatalf("Dockerfile source-revision contract missing %q", required)
		}
	}
	docs := readBuildContractFile(t, filepath.Join(root, "docs", "production-deployment.md"))
	for _, required := range []string{"exact accepted Git archive", "make docker-build", "fails closed and cannot"} {
		if !strings.Contains(docs, required) {
			t.Fatalf("production deployment source-revision contract missing %q", required)
		}
	}
}

func TestHostedCIUsesJobOwnedDatabaseWithoutSourceBindMount(t *testing.T) {
	// Hosted CI no longer needs the former private-runner Docker bind probe.
	// Preserve its actual safety property: test infrastructure never writes
	// artifacts into the checkout or binds the checkout into a database.
	workflow := readBuildContractFile(t, filepath.Join("..", ".github", "workflows", "ci.yml"))
	for _, required := range []string{
		"runs-on: ubuntu-24.04", "persist-credentials: false",
		"bash fork/scripts/ci-database.sh start", "bash fork/scripts/ci-database.sh stop",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("hosted CI contract missing %q", required)
		}
	}
	for _, forbidden := range []string{"runs-on: ags-go-ci", "self-hosted", "pull_request_target", "docker-bind-probe"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("private/privileged CI surface retained: %q", forbidden)
		}
	}
	database := readBuildContractFile(t, filepath.Join("..", "fork", "scripts", "ci-database.sh"))
	for _, required := range []string{"GitHub Actions only", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_JOB", "ags.ci.owner", "127.0.0.1:45400:4000", "Database owner mismatch; refusing stop"} {
		if !strings.Contains(database, required) {
			t.Fatalf("database ownership contract missing %q", required)
		}
	}
	// The executable process test inspects actual Docker argv (including
	// shorthand -v); a global substring would mistake command -v mysql for
	// a volume flag and test shell spelling rather than isolation.
	for _, forbidden := range []string{"--mount", "--volume", "${PWD}", "GITHUB_WORKSPACE"} {
		if strings.Contains(database, forbidden) {
			t.Fatalf("database may bind or write the checkout: %q", forbidden)
		}
	}
}

func TestExactSourceBuilderKeepsAuditRootOutsideCheckout(t *testing.T) {
	fixture, sha := exactSourceFixture(t, "INHERITED-TMPDIR")
	checkoutTMPDIR := filepath.Join(fixture, ".tmp", "workflow")
	if err := os.MkdirAll(checkoutTMPDIR, 0o755); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join("..", "scripts", "build-exact-source.sh")
	out, err := runExactSourceBuilder(script, fixture, sha, "docker", []string{
		"TMPDIR=" + checkoutTMPDIR,
		"AGS_EXACT_SOURCE_PREPARE_ONLY=1",
	})
	if err != nil {
		t.Fatalf("prepare with checkout-local TMPDIR: %v\n%s", err, out)
	}
	auditRoot := receiptValue(out, "audit_root")
	if auditRoot == "" {
		t.Fatalf("audit root receipt missing: %s", out)
	}
	assertPathOutsideRepo(t, auditRoot, fixture)
	assertPathOutsideRepo(t, receiptValue(out, "context"), fixture)
	if status := gitFixture(t, fixture, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("checkout changed by exact-source prepare: %s", status)
	}
}

func TestExactSourceBuilderRejectsCheckoutLocalExplicitAuditRootBeforeStaging(t *testing.T) {
	fixture, sha := exactSourceFixture(t, "EXPLICIT-TMPDIR")
	auditParent := filepath.Join(fixture, ".tmp", "audit")
	if err := os.MkdirAll(auditParent, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(auditParent)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "gh-server")
	script := filepath.Join("..", "scripts", "build-exact-source.sh")
	out, err := runExactSourceBuilder(script, fixture, sha, "binary", []string{
		"AGS_EXACT_SOURCE_TMPDIR=" + auditParent,
	}, "--output", output)
	if err == nil || !strings.Contains(out, "outside source repo") {
		t.Fatalf("checkout-local explicit audit root err=%v out=%s", err, out)
	}
	if strings.Contains(out, "audit_root=") {
		t.Fatalf("failed build published an audit receipt: %s", out)
	}
	after, readErr := os.ReadDir(auditParent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != len(before) {
		t.Fatalf("failed build staged audit artifacts: before=%d after=%d", len(before), len(after))
	}
	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Fatalf("failed build published output: %v", statErr)
	}
}

func TestExactSourceBuilderRejectsDirtyAndHiddenIndexDrift(t *testing.T) {
	fixture, sha := exactSourceFixture(t, "A")
	script := filepath.Join("..", "scripts", "build-exact-source.sh")

	if out, err := runExactSourceBuilder(script, fixture, sha, "verify", nil); err != nil {
		t.Fatalf("clean verify: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(fixture, "untracked"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runExactSourceBuilder(script, fixture, sha, "verify", nil); err == nil || !strings.Contains(out, "must be clean") {
		t.Fatalf("dirty verify err=%v out=%s", err, out)
	}
	if err := os.Remove(filepath.Join(fixture, "untracked")); err != nil {
		t.Fatal(err)
	}

	gitFixture(t, fixture, "update-index", "--assume-unchanged", "main.go")
	if err := os.WriteFile(filepath.Join(fixture, "main.go"), []byte("package main\nfunc main(){println(\"HIDDEN\")}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runExactSourceBuilder(script, fixture, sha, "verify", nil); err == nil || !strings.Contains(out, "index contains") {
		t.Fatalf("assume-unchanged verify err=%v out=%s", err, out)
	}
	gitFixture(t, fixture, "update-index", "--no-assume-unchanged", "main.go")
	gitFixture(t, fixture, "checkout", "--", "main.go")
	gitFixture(t, fixture, "update-index", "--skip-worktree", "main.go")
	if out, err := runExactSourceBuilder(script, fixture, sha, "verify", nil); err == nil || !strings.Contains(out, "index contains") {
		t.Fatalf("skip-worktree verify err=%v out=%s", err, out)
	}
}

func TestExactSourceBuilderRejectsSymlinkAndGitlinkLeavesBeforePublishing(t *testing.T) {
	script := filepath.Join("..", "scripts", "build-exact-source.sh")
	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "absolute symlink", target: filepath.Join(t.TempDir(), "outside.go")},
		{name: "relative symlink", target: "../../../outside.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, _ := exactSourceFixture(t, "LINK")
			if filepath.IsAbs(tc.target) {
				outside := "package main\nimport (\"fmt\"; \"github.com/ngaut/agent-git-service/server\")\nfunc main(){fmt.Print(\"OUTSIDE:\"+server.Revision())}\n"
				if err := os.WriteFile(tc.target, []byte(outside), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(fixture, "cmd", "gh-server", "main.go")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			gitFixture(t, fixture, "add", "cmd/gh-server/main.go")
			gitFixture(t, fixture, "commit", "-q", "-m", "tracked symlink")
			sha := strings.TrimSpace(gitFixture(t, fixture, "rev-parse", "HEAD"))
			output := filepath.Join(t.TempDir(), "gh-server")
			if out, err := runExactSourceBuilder(script, fixture, sha, "binary", nil, "--output", output); err == nil || !strings.Contains(out, "non-regular leaf") {
				t.Fatalf("symlink binary err=%v out=%s", err, out)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("binary output published: %v", err)
			}
			if out, err := runExactSourceBuilder(script, fixture, sha, "docker", []string{"AGS_EXACT_SOURCE_PREPARE_ONLY=1"}); err == nil || !strings.Contains(out, "non-regular leaf") || strings.Contains(out, "context=") {
				t.Fatalf("symlink docker prepare err=%v out=%s", err, out)
			}
		})
	}

	t.Run("gitlink", func(t *testing.T) {
		fixture, _ := exactSourceFixture(t, "GITLINK")
		object := strings.TrimSpace(gitFixture(t, fixture, "rev-parse", "HEAD"))
		nested := filepath.Join(fixture, "nested-module")
		clone := exec.Command("git", "clone", "-q", fixture, nested)
		if out, err := clone.CombinedOutput(); err != nil {
			t.Fatalf("clone nested gitlink fixture: %v\n%s", err, out)
		}
		gitFixture(t, nested, "checkout", "-q", object)
		gitFixture(t, fixture, "update-index", "--add", "--cacheinfo", "160000,"+object+",nested-module")
		gitFixture(t, fixture, "commit", "-q", "-m", "tracked gitlink")
		sha := strings.TrimSpace(gitFixture(t, fixture, "rev-parse", "HEAD"))
		output := filepath.Join(t.TempDir(), "gh-server")
		for _, mode := range []string{"verify", "binary", "docker"} {
			var args []string
			var env []string
			if mode == "binary" {
				args = []string{"--output", output}
			}
			if mode == "docker" {
				env = []string{"AGS_EXACT_SOURCE_PREPARE_ONLY=1"}
			}
			if out, err := runExactSourceBuilder(script, fixture, sha, mode, env, args...); err == nil || !strings.Contains(out, "non-regular leaf") || strings.Contains(out, "context=") {
				t.Fatalf("gitlink %s err=%v out=%s", mode, err, out)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("gitlink binary output published in %s mode: %v", mode, err)
			}
		}
	})
}

func TestExactSourceBuilderRejectsIllegalRegularLeafModeBeforePublishing(t *testing.T) {
	fixture, parent := exactSourceFixture(t, "ILLEGAL")
	parentTree := strings.TrimSpace(gitFixture(t, fixture, "rev-parse", parent+"^{tree}"))
	rawTree := gitFixture(t, fixture, "cat-file", "tree", parentTree)
	illegalTree := strings.Replace(rawTree, "100644 main.go\x00", "100664 main.go\x00", 1)
	if illegalTree == rawTree {
		t.Fatalf("main.go raw tree entry missing")
	}
	tree := strings.TrimSpace(gitFixtureInput(t, fixture, illegalTree, "hash-object", "-t", "tree", "--literally", "-w", "--stdin"))
	if tree == parentTree {
		t.Fatal("illegal raw tree unexpectedly retained the canonical tree ID")
	}
	rawCommit := "tree " + tree + "\nparent " + parent + "\nauthor Test <test@example.com> 0 +0000\ncommitter Test <test@example.com> 0 +0000\n\nillegal leaf mode\n"
	commit := strings.TrimSpace(gitFixtureInput(t, fixture, rawCommit, "hash-object", "-t", "commit", "--literally", "-w", "--stdin"))
	if got := strings.TrimSpace(gitFixture(t, fixture, "rev-parse", commit+"^{tree}")); got != tree {
		t.Fatalf("literal commit tree=%s want=%s", got, tree)
	}
	gitFixture(t, fixture, "update-ref", "HEAD", commit)
	gitFixture(t, fixture, "read-tree", commit)
	script := filepath.Join("..", "scripts", "build-exact-source.sh")
	for _, mode := range []string{"verify", "binary", "docker"} {
		output := filepath.Join(t.TempDir(), "gh-server")
		var args []string
		var env []string
		if mode == "binary" {
			args = []string{"--output", output}
		} else if mode == "docker" {
			env = []string{"AGS_EXACT_SOURCE_PREPARE_ONLY=1"}
		}
		out, err := runExactSourceBuilder(script, fixture, commit, mode, env, args...)
		if err == nil || !strings.Contains(out, "strict validation") || strings.Contains(out, "context=") {
			t.Fatalf("illegal leaf %s err=%v out=%s", mode, err, out)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("illegal leaf published output: %v", err)
		}
	}
}

func TestExactSourceBuilderRejectsCommitTreeAndCustomReplaceAuthorities(t *testing.T) {
	script := filepath.Join("..", "scripts", "build-exact-source.sh")
	for _, replaceTree := range []bool{false, true} {
		name := "commit"
		if replaceTree {
			name = "tree"
		}
		t.Run(name+" replace", func(t *testing.T) {
			fixture, shaA := exactSourceFixture(t, "A")
			if err := os.WriteFile(filepath.Join(fixture, "main.go"), []byte("package main\nfunc main(){println(\"B\")}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitFixture(t, fixture, "add", ".")
			gitFixture(t, fixture, "commit", "-q", "-m", "B")
			shaB := strings.TrimSpace(gitFixture(t, fixture, "rev-parse", "HEAD"))
			oldObject, newObject := shaA, shaB
			if replaceTree {
				oldObject = strings.TrimSpace(gitFixture(t, fixture, "rev-parse", shaA+"^{tree}"))
				newObject = strings.TrimSpace(gitFixture(t, fixture, "rev-parse", shaB+"^{tree}"))
			}
			gitFixture(t, fixture, "replace", oldObject, newObject)
			cmd := exec.Command("git", "-C", fixture, "checkout", "-q", shaA)
			cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("restore accepted checkout: %v\n%s", err, out)
			}
			for _, mode := range []string{"binary", "docker"} {
				output := filepath.Join(t.TempDir(), "gh-server")
				var args []string
				var env []string
				if mode == "binary" {
					args = []string{"--output", output}
				} else {
					env = []string{"AGS_EXACT_SOURCE_PREPARE_ONLY=1"}
				}
				out, err := runExactSourceBuilder(script, fixture, shaA, mode, env, args...)
				if err == nil || !strings.Contains(out, "replace refs") || strings.Contains(out, "context=") {
					t.Fatalf("%s replace %s err=%v out=%s", name, mode, err, out)
				}
				if _, err := os.Lstat(output); !os.IsNotExist(err) {
					t.Fatalf("replace build published output: %v", err)
				}
			}
		})
	}

	t.Run("custom replace base", func(t *testing.T) {
		fixture, sha := exactSourceFixture(t, "CUSTOM")
		out, err := runExactSourceBuilder(script, fixture, sha, "verify", []string{"GIT_REPLACE_REF_BASE=refs/custom-replace/"})
		if err == nil || !strings.Contains(out, "custom GIT_REPLACE_REF_BASE") {
			t.Fatalf("custom replace base err=%v out=%s", err, out)
		}
	})
}

func TestExactSourceDockerContextUsesVerifiedArchive(t *testing.T) {
	fixture, sha := exactSourceFixture(t, "DOCKER")
	script := filepath.Join("..", "scripts", "build-exact-source.sh")
	out, err := runExactSourceBuilder(script, fixture, sha, "docker", []string{"AGS_EXACT_SOURCE_PREPARE_ONLY=1"})
	if err != nil {
		t.Fatalf("prepare docker context: %v\n%s", err, out)
	}
	var contextDir string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "context=") {
			contextDir = strings.TrimPrefix(line, "context=")
		}
	}
	if contextDir == "" {
		t.Fatalf("context receipt missing: %s", out)
	}
	if got := strings.TrimSpace(readBuildContractFile(t, filepath.Join(contextDir, ".ags-source-revision"))); got != sha {
		t.Fatalf("docker source marker=%q want=%q", got, sha)
	}
	if got := readBuildContractFile(t, filepath.Join(contextDir, "main.go")); !strings.Contains(got, "DOCKER:") {
		t.Fatalf("docker context did not come from accepted archive: %s", got)
	}
}

func TestExactSourceBuilderPinsArchiveAcrossHeadABAAndInjectsRevision(t *testing.T) {
	fixture, shaA := exactSourceFixture(t, "A")
	if err := os.WriteFile(filepath.Join(fixture, "main.go"), []byte("package main\nimport (\"fmt\"; \"github.com/ngaut/agent-git-service/server\")\nfunc main(){fmt.Print(\"B:\"+server.Revision())}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, fixture, "add", ".")
	gitFixture(t, fixture, "commit", "-m", "B")
	shaB := strings.TrimSpace(gitFixture(t, fixture, "rev-parse", "HEAD"))
	gitFixture(t, fixture, "checkout", "-q", shaA)
	output := filepath.Join(t.TempDir(), "gh-server")
	hook := "git -C " + shellQuote(fixture) + " checkout -q " + shaB + " && git -C " + shellQuote(fixture) + " checkout -q " + shaA
	extra := []string{"AGS_EXACT_SOURCE_TEST_MODE=1", "AGS_EXACT_SOURCE_AFTER_VERIFY_HOOK=" + hook}
	script := filepath.Join("..", "scripts", "build-exact-source.sh")
	if out, err := runExactSourceBuilder(script, fixture, shaA, "binary", append(extra, "AGS_EXACT_SOURCE_OUTPUT="+output), "--output", output); err != nil {
		t.Fatalf("ABA build: %v\n%s", err, out)
	}
	result, err := exec.Command(output).CombinedOutput()
	if err != nil {
		t.Fatalf("run built fixture: %v %s", err, result)
	}
	if got, want := string(result), "A:"+shaA; got != want {
		t.Fatalf("built output=%q want=%q", got, want)
	}
}

func exactSourceFixture(t *testing.T, marker string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		".gitignore":            ".tmp/\n",
		"go.mod":                "module github.com/ngaut/agent-git-service\n\ngo 1.25\n",
		"main.go":               "package main\nimport (\"fmt\"; \"github.com/ngaut/agent-git-service/server\")\nfunc main(){fmt.Print(\"" + marker + ":\"+server.Revision())}\n",
		"cmd/gh-server/main.go": "package main\nimport (\"fmt\"; \"github.com/ngaut/agent-git-service/server\")\nfunc main(){fmt.Print(\"" + marker + ":\"+server.Revision())}\n",
		"server/revision.go":    "package server\nvar gitSHA=\"unknown\"\nfunc Revision() string{return gitSHA}\n",
		"Dockerfile":            "FROM scratch\n",
	}
	for name, value := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitFixture(t, dir, "init", "-q")
	gitFixture(t, dir, "config", "user.email", "test@example.com")
	gitFixture(t, dir, "config", "user.name", "Test")
	gitFixture(t, dir, "add", ".")
	gitFixture(t, dir, "commit", "-q", "-m", marker)
	return dir, strings.TrimSpace(gitFixture(t, dir, "rev-parse", "HEAD"))
}

func runExactSourceBuilder(script, repo, sha, mode string, extraEnv []string, args ...string) (string, error) {
	argv := append([]string{script, mode}, args...)
	cmd := exec.Command("bash", argv...)
	cmd.Env = append(os.Environ(), "AGS_SOURCE_REPO="+repo, "GIT_SHA="+sha)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gitFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitFixtureInput(t, dir, "", args...)
}

func gitFixtureInput(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func receiptValue(receipt, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(receipt, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func assertPathOutsideRepo(t *testing.T, path, repo string) {
	t.Helper()
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("canonicalize path %q: %v", path, err)
	}
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("canonicalize repo %q: %v", repo, err)
	}
	rel, err := filepath.Rel(canonicalRepo, canonicalPath)
	if err != nil {
		t.Fatalf("relate path %q to repo %q: %v", canonicalPath, canonicalRepo, err)
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		t.Fatalf("path %q is within source repo %q", canonicalPath, canonicalRepo)
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func readBuildContractFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
