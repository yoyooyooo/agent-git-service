package artifactretirement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const IntentSchema = "ags.artifact-retirement.intent.v1"

type BlobSpec struct {
	OID    string `json:"oid"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type Intent struct {
	Schema                string     `json:"schema"`
	OperationID           string     `json:"operation_id"`
	Repository            string     `json:"repository"`
	DefaultBranch         string     `json:"default_branch"`
	ExpectedAncestor      string     `json:"expected_ancestor"`
	RecoveryArchive       string     `json:"recovery_archive"`
	RecoveryArchiveSHA256 string     `json:"recovery_archive_sha256"`
	AllowSignatureRemoval bool       `json:"allow_signature_removal"`
	RetiredBlobs          []BlobSpec `json:"retired_blobs"`
}

type RefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

type Plan struct {
	OperationID       string            `json:"operation_id"`
	Repository        string            `json:"repository"`
	DefaultBranch     string            `json:"default_branch"`
	OriginalHead      string            `json:"original_head"`
	CleanHead         string            `json:"clean_head"`
	DefaultTree       string            `json:"default_tree"`
	CommitMap         map[string]string `json:"commit_map"`
	OriginalRefs      map[string]string `json:"original_refs"`
	CleanRefs         map[string]string `json:"clean_refs"`
	RefUpdates        []RefUpdate       `json:"ref_updates"`
	RetiredBlobs      []BlobSpec        `json:"retired_blobs"`
	SignatureRemovals int               `json:"signature_removals"`
	BeforeDiskKiB     int64             `json:"before_disk_kib"`
	StagingGitDir     string            `json:"staging_git_dir"`
}

type gitRunner struct{ dir string }

func (g gitRunner) run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "--git-dir=" + g.dir}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = sanitizedGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s failed: %w", firstArg(args), err)
	}
	return stdout.Bytes(), nil
}

func runGit(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = sanitizedGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s failed: %w", firstArg(args), err)
	}
	return stdout.Bytes(), nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return "command"
	}
	return args[0]
}

func sanitizedGitEnv() []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"TMPDIR": true, "TMP": true, "TEMP": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "SYSTEMROOT": true, "WINDIR": true,
	}
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if allowed[key] {
			env = append(env, item)
		}
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_NO_LAZY_FETCH=1",
	)
}

func LoadIntent(path string) (Intent, error) {
	var intent Intent
	st, err := os.Lstat(path)
	if err != nil {
		return intent, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Size() > 64*1024 {
		return intent, errors.New("artifact retirement intent must be a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return intent, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&intent); err != nil {
		return intent, errors.New("invalid artifact retirement intent")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return intent, errors.New("invalid artifact retirement intent")
	}
	if err := validateIntent(intent); err != nil {
		return intent, err
	}
	return intent, nil
}

func validateIntent(in Intent) error {
	if in.Schema != IntentSchema || !safeID(in.OperationID) || !safeRepo(in.Repository) || !safeBranch(in.DefaultBranch) || !fullOID(in.ExpectedAncestor) {
		return errors.New("invalid artifact retirement intent identity")
	}
	if strings.TrimSpace(in.RecoveryArchive) == "" || !fullDigest(in.RecoveryArchiveSHA256) || !in.AllowSignatureRemoval {
		return errors.New("artifact retirement recovery/signature acknowledgement is required")
	}
	if len(in.RetiredBlobs) == 0 || len(in.RetiredBlobs) > 32 {
		return errors.New("artifact retirement blob set is out of bounds")
	}
	seen := map[string]bool{}
	for _, blob := range in.RetiredBlobs {
		if !fullOID(blob.OID) || blob.Bytes <= 0 || blob.Bytes > 256*1024*1024 || !fullDigest(blob.SHA256) || seen[blob.OID] {
			return errors.New("invalid artifact retirement blob specification")
		}
		seen[blob.OID] = true
	}
	return nil
}

func safeID(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func safeRepo(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && safeID(parts[0]) && safeID(parts[1])
}

func safeBranch(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, " ~^:?*[\\\x00\r\n") || strings.Contains(value, "..") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".lock") {
		return false
	}
	return true
}

func fullOID(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func fullDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func Prepare(ctx context.Context, sourceGitDir, workRoot string, intent Intent) (Plan, error) {
	var plan Plan
	if err := validateIntent(intent); err != nil {
		return plan, err
	}
	source, err := filepath.EvalSymlinks(sourceGitDir)
	if err != nil {
		return plan, err
	}
	sourceAbs, err := filepath.Abs(sourceGitDir)
	if err != nil || source != sourceAbs {
		return plan, errors.New("artifact retirement source repository must be canonical")
	}
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return plan, err
	}
	workRootAbs, err := filepath.Abs(workRoot)
	if err != nil {
		return plan, err
	}
	staging := filepath.Join(workRootAbs, "staging.git")
	if err := os.RemoveAll(staging); err != nil {
		return plan, err
	}
	src := gitRunner{dir: source}
	headRef := "refs/heads/" + intent.DefaultBranch
	headOut, err := src.run(ctx, nil, "rev-parse", "--verify", headRef+"^{commit}")
	if err != nil {
		return plan, errors.New("artifact retirement default branch is unavailable")
	}
	originalHead := strings.TrimSpace(string(headOut))
	if !fullOID(originalHead) {
		return plan, errors.New("artifact retirement default branch is invalid")
	}
	if _, err := src.run(ctx, nil, "merge-base", "--is-ancestor", intent.ExpectedAncestor, originalHead); err != nil {
		return plan, errors.New("artifact retirement expected ancestor is not on the current default branch")
	}
	treeOut, err := src.run(ctx, nil, "rev-parse", originalHead+"^{tree}")
	if err != nil {
		return plan, err
	}
	defaultTree := strings.TrimSpace(string(treeOut))
	retired := map[string]BlobSpec{}
	for _, spec := range intent.RetiredBlobs {
		retired[spec.OID] = spec
		if err := verifyBlob(ctx, src, spec); err != nil {
			return plan, err
		}
	}
	if err := ensureTreeDoesNotUseRetired(ctx, src, originalHead, retired); err != nil {
		return plan, err
	}
	if _, err := runGit(ctx, nil, "clone", "--mirror", "--no-local", source, staging); err != nil {
		return plan, errors.New("artifact retirement staging clone failed")
	}
	stage := gitRunner{dir: staging}
	if refs, _ := stage.run(ctx, nil, "for-each-ref", "--format=%(refname)", "refs/replace/"); strings.TrimSpace(string(refs)) != "" {
		return plan, errors.New("artifact retirement refuses a repository with existing replace refs")
	}
	commits, err := stage.run(ctx, nil, "rev-list", "--topo-order", "--reverse", "--all", "--parents")
	if err != nil {
		return plan, err
	}
	commitMap := map[string]string{}
	treeMemo := map[string]string{}
	signatureRemovals := 0
	for _, line := range strings.Split(strings.TrimSpace(string(commits)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		old := fields[0]
		newOID, removed, err := rewriteCommit(ctx, stage, old, commitMap, treeMemo, retired, intent.AllowSignatureRemoval)
		if err != nil {
			return plan, err
		}
		commitMap[old] = newOID
		signatureRemovals += removed
	}
	refs, err := listRefs(ctx, stage)
	if err != nil {
		return plan, err
	}
	originalRefs := make(map[string]string, len(refs))
	for _, ref := range refs {
		originalRefs[ref.Name] = ref.OID
	}
	tagMemo := map[string]string{}
	var updates []RefUpdate
	for _, ref := range refs {
		newOID := ref.OID
		switch ref.Type {
		case "commit":
			if mapped := commitMap[ref.OID]; mapped != "" {
				newOID = mapped
			}
		case "tag":
			var removed int
			newOID, removed, err = rewriteTag(ctx, stage, ref.OID, commitMap, tagMemo, retired, intent.AllowSignatureRemoval)
			if err != nil {
				return plan, err
			}
			signatureRemovals += removed
		case "tree":
		case "blob":
			if _, retiredBlob := retired[ref.OID]; retiredBlob {
				return plan, fmt.Errorf("ref %s points directly at a retired blob", ref.Name)
			}
		default:
			return plan, fmt.Errorf("unsupported ref target type %s", ref.Type)
		}
		if strings.HasPrefix(ref.Name, "refs/ags/retention/") {
			suffix := strings.TrimPrefix(ref.Name, "refs/ags/retention/")
			if suffix != ref.OID {
				return plan, errors.New("artifact retirement found an invalid retention ref")
			}
			if newOID != ref.OID {
				if _, err := stage.run(ctx, nil, "update-ref", "-d", ref.Name, ref.OID); err != nil {
					return plan, err
				}
				updates = append(updates, RefUpdate{Ref: ref.Name, Old: ref.OID})
				newRef := "refs/ags/retention/" + newOID
				if existing, ok := originalRefs[newRef]; ok {
					if existing != newOID {
						return plan, errors.New("artifact retirement retention ref collision")
					}
				} else {
					if _, err := stage.run(ctx, nil, "update-ref", newRef, newOID, ""); err != nil {
						return plan, err
					}
					updates = append(updates, RefUpdate{Ref: newRef, New: newOID})
				}
			}
			continue
		}
		if newOID != ref.OID {
			if _, err := stage.run(ctx, nil, "update-ref", ref.Name, newOID, ref.OID); err != nil {
				return plan, err
			}
			updates = append(updates, RefUpdate{Ref: ref.Name, Old: ref.OID, New: newOID})
		}
	}
	for old, newOID := range commitMap {
		if old == newOID {
			continue
		}
		if _, err := stage.run(ctx, nil, "update-ref", "refs/replace/"+old, newOID); err != nil {
			return plan, err
		}
	}
	cleanHeadOut, err := stage.run(ctx, nil, "rev-parse", "--verify", headRef+"^{commit}")
	if err != nil {
		return plan, err
	}
	cleanHead := strings.TrimSpace(string(cleanHeadOut))
	cleanTreeOut, err := stage.run(ctx, nil, "rev-parse", cleanHead+"^{tree}")
	if err != nil {
		return plan, err
	}
	if strings.TrimSpace(string(cleanTreeOut)) != defaultTree {
		return plan, errors.New("artifact retirement changed the current default-branch tree")
	}
	if _, err := stage.run(ctx, nil, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"); err != nil {
		return plan, err
	}
	if _, err := stage.run(ctx, nil, "gc", "--prune=now"); err != nil {
		return plan, err
	}
	for _, spec := range intent.RetiredBlobs {
		if objectPhysicallyExists(ctx, staging, spec.OID) {
			return plan, fmt.Errorf("retired blob %s remains reachable in staging", spec.OID)
		}
	}
	if _, err := stage.run(ctx, nil, "fsck", "--full", "--no-dangling"); err != nil {
		return plan, err
	}
	cleanRefRows, err := listRefs(ctx, stage)
	if err != nil {
		return plan, err
	}
	cleanRefs := make(map[string]string, len(cleanRefRows))
	for _, ref := range cleanRefRows {
		cleanRefs[ref.Name] = ref.OID
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Ref < updates[j].Ref })
	return Plan{
		OperationID: intent.OperationID, Repository: intent.Repository, DefaultBranch: intent.DefaultBranch,
		OriginalHead: originalHead, CleanHead: cleanHead, DefaultTree: defaultTree,
		CommitMap: commitMap, OriginalRefs: originalRefs, CleanRefs: cleanRefs, RefUpdates: updates,
		RetiredBlobs: append([]BlobSpec(nil), intent.RetiredBlobs...), SignatureRemovals: signatureRemovals, StagingGitDir: staging,
	}, nil
}

type refInfo struct {
	Name, OID, Type string
}

func listRefs(ctx context.Context, g gitRunner) ([]refInfo, error) {
	out, err := g.run(ctx, nil, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(objecttype)")
	if err != nil {
		return nil, err
	}
	var refs []refInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 || strings.HasPrefix(parts[0], "refs/replace/") || strings.HasPrefix(parts[0], "refs/retirement-import/") {
			if len(parts) == 3 && (strings.HasPrefix(parts[0], "refs/replace/") || strings.HasPrefix(parts[0], "refs/retirement-import/")) {
				continue
			}
			return nil, errors.New("invalid artifact retirement ref inventory")
		}
		refs = append(refs, refInfo{Name: parts[0], OID: parts[1], Type: parts[2]})
	}
	return refs, nil
}

func verifyBlob(ctx context.Context, g gitRunner, spec BlobSpec) error {
	typeOut, err := g.run(ctx, nil, "cat-file", "-t", spec.OID)
	if err != nil || strings.TrimSpace(string(typeOut)) != "blob" {
		return fmt.Errorf("retired object %s is not an available blob", spec.OID)
	}
	sizeOut, err := g.run(ctx, nil, "cat-file", "-s", spec.OID)
	if err != nil {
		return err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64)
	if err != nil || size != spec.Bytes {
		return fmt.Errorf("retired blob %s size does not match intent", spec.OID)
	}
	body, err := g.run(ctx, nil, "cat-file", "blob", spec.OID)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != spec.SHA256 {
		return fmt.Errorf("retired blob %s digest does not match intent", spec.OID)
	}
	return nil
}

func ensureTreeDoesNotUseRetired(ctx context.Context, g gitRunner, commit string, retired map[string]BlobSpec) error {
	out, err := g.run(ctx, nil, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return err
	}
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 {
			return errors.New("invalid current tree inventory")
		}
		fields := strings.Fields(string(record[:tab]))
		if len(fields) < 3 {
			return errors.New("invalid current tree inventory")
		}
		if _, found := retired[fields[2]]; found {
			return errors.New("current default-branch tree still uses a retired blob")
		}
	}
	return nil
}

func objectPhysicallyExists(ctx context.Context, gitDir, oid string) bool {
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "cat-file", "-e", oid+"^{object}")
	cmd.Env = append(sanitizedGitEnv(), "GIT_NO_REPLACE_OBJECTS=1")
	return cmd.Run() == nil
}

func rewriteTree(ctx context.Context, g gitRunner, oid string, memo map[string]string, retired map[string]BlobSpec) (string, error) {
	if mapped, ok := memo[oid]; ok {
		return mapped, nil
	}
	out, err := g.run(ctx, nil, "ls-tree", "-z", oid)
	if err != nil {
		return "", err
	}
	changed := false
	var rebuilt bytes.Buffer
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 {
			return "", errors.New("invalid tree entry")
		}
		meta, name := string(record[:tab]), record[tab+1:]
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return "", errors.New("invalid tree entry metadata")
		}
		mode, typ, child := fields[0], fields[1], fields[2]
		switch typ {
		case "blob":
			if _, remove := retired[child]; remove {
				changed = true
				continue
			}
		case "tree":
			next, err := rewriteTree(ctx, g, child, memo, retired)
			if err != nil {
				return "", err
			}
			if next != child {
				changed = true
				child = next
			}
			if child == "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
				changed = true
				continue
			}
		case "commit":
		default:
			return "", fmt.Errorf("unsupported tree entry type %s", typ)
		}
		fmt.Fprintf(&rebuilt, "%s %s %s\t", mode, typ, child)
		rebuilt.Write(name)
		rebuilt.WriteByte(0)
	}
	if !changed {
		memo[oid] = oid
		return oid, nil
	}
	newTree, err := g.run(ctx, rebuilt.Bytes(), "mktree", "-z")
	if err != nil {
		return "", err
	}
	mapped := strings.TrimSpace(string(newTree))
	memo[oid] = mapped
	return mapped, nil
}

type headerGroup struct {
	key   string
	lines []string
}

func parseObject(raw []byte) ([]headerGroup, []byte, error) {
	parts := bytes.SplitN(raw, []byte("\n\n"), 2)
	if len(parts) != 2 {
		return nil, nil, errors.New("invalid Git object header")
	}
	var groups []headerGroup
	for _, line := range strings.Split(string(parts[0]), "\n") {
		if strings.HasPrefix(line, " ") {
			if len(groups) == 0 {
				return nil, nil, errors.New("invalid Git object continuation")
			}
			groups[len(groups)-1].lines = append(groups[len(groups)-1].lines, line)
			continue
		}
		key := line
		if at := strings.IndexByte(line, ' '); at >= 0 {
			key = line[:at]
		}
		groups = append(groups, headerGroup{key: key, lines: []string{line}})
	}
	return groups, parts[1], nil
}

func rewriteCommit(ctx context.Context, g gitRunner, old string, commitMap, treeMemo map[string]string, retired map[string]BlobSpec, allowSignatures bool) (string, int, error) {
	raw, err := g.run(ctx, nil, "cat-file", "commit", old)
	if err != nil {
		return "", 0, err
	}
	groups, message, err := parseObject(raw)
	if err != nil {
		return "", 0, err
	}
	changed := false
	for _, group := range groups {
		if group.key == "tree" {
			fields := strings.Fields(group.lines[0])
			if len(fields) != 2 {
				return "", 0, errors.New("invalid commit tree header")
			}
			next, err := rewriteTree(ctx, g, fields[1], treeMemo, retired)
			if err != nil {
				return "", 0, err
			}
			if next != fields[1] {
				changed = true
			}
		}
		if group.key == "parent" {
			fields := strings.Fields(group.lines[0])
			if len(fields) != 2 {
				return "", 0, errors.New("invalid commit parent header")
			}
			if mapped := commitMap[fields[1]]; mapped != "" && mapped != fields[1] {
				changed = true
			}
		}
	}
	if !changed {
		return old, 0, nil
	}
	var rebuilt bytes.Buffer
	removed := 0
	for _, group := range groups {
		if group.key == "gpgsig" || group.key == "gpgsig-sha256" || group.key == "mergetag" {
			if !allowSignatures {
				return "", 0, errors.New("artifact retirement would invalidate a signed commit")
			}
			removed++
			continue
		}
		line := group.lines[0]
		if group.key == "tree" {
			fields := strings.Fields(line)
			next, err := rewriteTree(ctx, g, fields[1], treeMemo, retired)
			if err != nil {
				return "", 0, err
			}
			line = "tree " + next
		} else if group.key == "parent" {
			fields := strings.Fields(line)
			if mapped := commitMap[fields[1]]; mapped != "" {
				line = "parent " + mapped
			}
		}
		rebuilt.WriteString(line)
		rebuilt.WriteByte('\n')
		for _, continuation := range group.lines[1:] {
			rebuilt.WriteString(continuation)
			rebuilt.WriteByte('\n')
		}
	}
	rebuilt.WriteByte('\n')
	rebuilt.Write(message)
	newOID, err := g.run(ctx, rebuilt.Bytes(), "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(string(newOID)), removed, nil
}

func rewriteTag(ctx context.Context, g gitRunner, old string, commitMap, tagMemo map[string]string, retired map[string]BlobSpec, allowSignatures bool) (string, int, error) {
	if mapped, ok := tagMemo[old]; ok {
		return mapped, 0, nil
	}
	raw, err := g.run(ctx, nil, "cat-file", "tag", old)
	if err != nil {
		return "", 0, err
	}
	groups, message, err := parseObject(raw)
	if err != nil {
		return "", 0, err
	}
	target, typ := "", ""
	for _, group := range groups {
		fields := strings.Fields(group.lines[0])
		if group.key == "object" && len(fields) == 2 {
			target = fields[1]
		}
		if group.key == "type" && len(fields) == 2 {
			typ = fields[1]
		}
	}
	if !fullOID(target) || typ == "" {
		return "", 0, errors.New("invalid annotated tag")
	}
	newTarget := target
	removed := 0
	switch typ {
	case "commit":
		if mapped := commitMap[target]; mapped != "" {
			newTarget = mapped
		}
	case "tag":
		newTarget, removed, err = rewriteTag(ctx, g, target, commitMap, tagMemo, retired, allowSignatures)
		if err != nil {
			return "", 0, err
		}
	case "blob":
		if _, found := retired[target]; found {
			return "", 0, errors.New("annotated tag points directly at a retired blob")
		}
	case "tree":
	default:
		return "", 0, fmt.Errorf("unsupported annotated tag target type %s", typ)
	}
	if newTarget == target {
		tagMemo[old] = old
		return old, removed, nil
	}
	if !allowSignatures && (bytes.Contains(message, []byte("-----BEGIN PGP SIGNATURE-----")) || bytes.Contains(message, []byte("-----BEGIN SSH SIGNATURE-----"))) {
		return "", 0, errors.New("artifact retirement would invalidate a signed tag")
	}
	for _, marker := range [][]byte{[]byte("-----BEGIN PGP SIGNATURE-----"), []byte("-----BEGIN SSH SIGNATURE-----")} {
		if at := bytes.Index(message, marker); at >= 0 {
			message = bytes.TrimRight(message[:at], "\n")
			message = append(message, '\n')
			removed++
		}
	}
	var rebuilt bytes.Buffer
	for _, group := range groups {
		line := group.lines[0]
		if group.key == "object" {
			line = "object " + newTarget
		}
		rebuilt.WriteString(line)
		rebuilt.WriteByte('\n')
		for _, continuation := range group.lines[1:] {
			rebuilt.WriteString(continuation)
			rebuilt.WriteByte('\n')
		}
	}
	rebuilt.WriteByte('\n')
	rebuilt.Write(message)
	newOID, err := g.run(ctx, rebuilt.Bytes(), "hash-object", "-t", "tag", "-w", "--stdin")
	if err != nil {
		return "", 0, err
	}
	mapped := strings.TrimSpace(string(newOID))
	tagMemo[old] = mapped
	return mapped, removed, nil
}
