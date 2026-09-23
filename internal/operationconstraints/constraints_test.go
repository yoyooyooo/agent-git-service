package operationconstraints_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/operationconstraints"
)

func TestDefaultOperationConstraintSchemas(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	refDigest := strings.Repeat("c", 64)
	cases := []struct {
		operation string
		input     map[string]any
		want      map[string]string
	}{
		{"repo.read", map[string]any{}, map[string]string{}},
		{"repo.create", map[string]any{"target_repository": "operator/new-repo", "base_ref": "main", "source_base_sha": head, "source_ref_digest": refDigest, "import_mode": "ags-forgejo", "visibility": "private"}, map[string]string{"target_repository": "operator/new-repo", "base_ref": "main", "source_base_sha": head, "source_ref_digest": refDigest, "import_mode": "ags-forgejo", "visibility": "private"}},
		{"repo.admin", map[string]any{"target_repository": "operator/new-repo", "base_ref": "main", "source_base_sha": head, "source_ref_digest": refDigest, "action": "forgejo_onboard"}, map[string]string{"target_repository": "operator/new-repo", "base_ref": "main", "source_base_sha": head, "source_ref_digest": refDigest, "action": "forgejo_onboard"}},
		{"git.read", map[string]any{}, map[string]string{}},
		{"git.push", map[string]any{}, map[string]string{}},
		{"pr.create", map[string]any{"base_ref": "main", "head_ref": "agent/change"}, map[string]string{"base_ref": "main", "head_ref": "agent/change"}},
		{"pr.rebase", map[string]any{"pull_request_number": json.Number("27"), "forgejo_pull_request_number": json.Number("42"), "expected_head_sha": head, "expected_base_sha": base}, map[string]string{"pull_request_number": "27", "forgejo_pull_request_number": "42", "expected_head_sha": head, "expected_base_sha": base}},
		{"pr.merge", map[string]any{"pull_request_number": json.Number("27"), "forgejo_pull_request_number": json.Number("42"), "expected_head_sha": head, "merge_method": "fast-forward-only"}, map[string]string{"pull_request_number": "27", "forgejo_pull_request_number": "42", "expected_head_sha": head, "merge_method": "fast-forward-only"}},
		{"pr.read", map[string]any{"pull_request_number": json.Number("27")}, map[string]string{"pull_request_number": "27"}},
		{"pr.read", map[string]any{"head_ref": "agent/change"}, map[string]string{"head_ref": "agent/change"}},
		{"pr.read", map[string]any{}, map[string]string{}},
		{"review.read", map[string]any{"pull_request_number": json.Number("27"), "forgejo_pull_request_number": json.Number("42")}, map[string]string{"pull_request_number": "27", "forgejo_pull_request_number": "42"}},
		{"review.submit", map[string]any{"pull_request_number": json.Number("27"), "forgejo_pull_request_number": json.Number("42"), "review_action": "approve"}, map[string]string{"pull_request_number": "27", "forgejo_pull_request_number": "42", "review_action": "approve"}},
		{"ci.read", map[string]any{}, map[string]string{}},
		{"ci.read", map[string]any{"run_id": json.Number("99")}, map[string]string{"run_id": "99"}},
		{"ci.read", map[string]any{"pull_request_number": json.Number("27"), "forgejo_pull_request_number": json.Number("42")}, map[string]string{"pull_request_number": "27", "forgejo_pull_request_number": "42"}},
		{"ci.read", map[string]any{"pull_request_number": json.Number("27"), "forgejo_pull_request_number": json.Number("42"), "head_sha": head}, map[string]string{"pull_request_number": "27", "forgejo_pull_request_number": "42", "head_sha": head}},
	}
	for _, tc := range cases {
		t.Run(tc.operation+strings.Join(mapsKeys(tc.want), "-"), func(t *testing.T) {
			got, err := operationconstraints.NormalizeJSON(tc.operation, tc.input)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NormalizeJSON()=%#v err=%v want=%#v", got, err, tc.want)
			}
			if !operationconstraints.Match(tc.operation, tc.want, got) {
				t.Fatal("normalized persisted/use-time forms did not match")
			}
		})
	}
}

func TestDefaultOperationConstraintTamperFailsClosed(t *testing.T) {
	head := strings.Repeat("a", 40)
	refDigest := strings.Repeat("c", 64)
	for name, tc := range map[string]struct {
		operation string
		input     map[string]any
	}{
		"repo constraint":          {"repo.read", map[string]any{"head_ref": "main"}},
		"repo create missing pin":  {"repo.create", map[string]any{"target_repository": "operator/new-repo", "base_ref": "main", "source_base_sha": head, "import_mode": "ags-forgejo", "visibility": "private"}},
		"repo create unsafe owner": {"repo.create", map[string]any{"target_repository": "../operator/new-repo", "base_ref": "main", "source_base_sha": head, "source_ref_digest": refDigest, "import_mode": "ags-forgejo", "visibility": "private"}},
		"repo create unknown mode": {"repo.create", map[string]any{"target_repository": "operator/new-repo", "base_ref": "main", "source_base_sha": head, "source_ref_digest": refDigest, "import_mode": "mirror-all", "visibility": "private"}},
		"repo admin wrong action":  {"repo.admin", map[string]any{"target_repository": "operator/new-repo", "base_ref": "main", "source_base_sha": head, "source_ref_digest": refDigest, "action": "delete"}},
		"git read constraint":      {"git.read", map[string]any{"head_ref": "main"}},
		"git push constraint":      {"git.push", map[string]any{"head_ref": "main"}},
		"create empty":             {"pr.create", map[string]any{}},
		"create subset":            {"pr.create", map[string]any{"base_ref": "main"}},
		"create invalid ref":       {"pr.create", map[string]any{"base_ref": "refs/tags/main", "head_ref": "agent/change"}},
		"rebase old exact head":    {"pr.rebase", map[string]any{"pull_request_number": 27, "forgejo_pull_request_number": 42, "exact_head": head, "expected_base_sha": head}},
		"merge missing method":     {"pr.merge", map[string]any{"pull_request_number": 27, "forgejo_pull_request_number": 42, "expected_head_sha": head}},
		"merge unknown method":     {"pr.merge", map[string]any{"pull_request_number": 27, "forgejo_pull_request_number": 42, "expected_head_sha": head, "merge_method": "octopus"}},
		"merge uppercase head":     {"pr.merge", map[string]any{"pull_request_number": 27, "forgejo_pull_request_number": 42, "expected_head_sha": strings.ToUpper(head), "merge_method": "rebase"}},
		"pr read mixed selectors":  {"pr.read", map[string]any{"pull_request_number": 27, "head_ref": "agent/change"}},
		"pr read unsafe number":    {"pr.read", map[string]any{"pull_request_number": float64(9007199254740992)}},
		"pr read string number":    {"pr.read", map[string]any{"pull_request_number": "27"}},
		"review missing provider":  {"review.read", map[string]any{"pull_request_number": 27}},
		"review exact head":        {"review.read", map[string]any{"pull_request_number": 27, "forgejo_pull_request_number": 42, "exact_head": true}},
		"review submit unknown":    {"review.submit", map[string]any{"pull_request_number": 27, "forgejo_pull_request_number": 42, "review_action": "merge"}},
		"ci sha only":              {"ci.read", map[string]any{"head_sha": head}},
		"ci event":                 {"ci.read", map[string]any{"event": "push"}},
		"ci event with sha":        {"ci.read", map[string]any{"event": "push", "head_sha": head}},
		"ci mixed run and PR":      {"ci.read", map[string]any{"run_id": 99, "pull_request_number": 27, "forgejo_pull_request_number": 42}},
		"ci unsafe run":            {"ci.read", map[string]any{"run_id": float64(9007199254740992)}},
		"ci string run":            {"ci.read", map[string]any{"run_id": "99"}},
		"ci old exact head":        {"ci.read", map[string]any{"pull_request_number": 27, "forgejo_pull_request_number": 42, "exact_head": true}},
		"ci uppercase head":        {"ci.read", map[string]any{"pull_request_number": 27, "forgejo_pull_request_number": 42, "head_sha": strings.ToUpper(head)}},
		"secret shaped constraint": {"pr.read", map[string]any{"head_ref": "ags_sess_forbidden"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := operationconstraints.NormalizeJSON(tc.operation, tc.input); err == nil {
				t.Fatal("tampered constraint vector accepted")
			}
		})
	}
}

func TestConstraintMatchUsesSameSafeIntegerAndRefNormalization(t *testing.T) {
	if !operationconstraints.Match("pr.create",
		map[string]string{"base_ref": "main", "head_ref": "refs/heads/agent/change"},
		map[string]string{"base_ref": "refs/heads/main", "head_ref": "agent/change"}) {
		t.Fatal("equivalent canonical branch refs did not match")
	}
	if operationconstraints.Match("pr.read", map[string]string{"pull_request_number": "27"}, map[string]string{"pull_request_number": "027"}) {
		t.Fatal("non-canonical integer matched")
	}
}

func mapsKeys(input map[string]string) []string {
	out := make([]string, 0, len(input))
	for key := range input {
		out = append(out, key)
	}
	return out
}
