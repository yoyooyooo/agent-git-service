package providerlogbridge

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCIBridgeBindsBranchRunWithoutInventingProviderPR(t *testing.T) {
	head := strings.Repeat("a", 40)
	parse := func(pr, ref, sha string) (expectedBinding, error) {
		values := url.Values{"provider_pr": {pr}, "head_ref": {ref}, "head_sha": {sha}}
		return parseExpectedBinding(httptest.NewRequest("GET", "/logs?"+values.Encode(), nil))
	}
	evidence := func(ref, sha, event string) logBinding {
		return logBinding{CommitSHA: sha, JobCommitSHA: sha, TaskCommitSHA: sha, ProviderRef: ref, Event: event}
	}
	branch, err := parse("0", "feature", head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifyBinding(evidence("refs/heads/feature", head, "push"), branch); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []logBinding{
		evidence("refs/pull/0/head", head, "pull_request"),
		evidence("refs/heads/other", head, "push"),
		evidence("refs/heads/feature", strings.Repeat("b", 40), "push"),
	} {
		if _, err = verifyBinding(invalid, branch); err == nil {
			t.Fatal("mismatched branch evidence accepted")
		}
	}
	// All three stored commit bindings must agree, not just the run-level SHA.
	for _, field := range []string{"run", "job", "task"} {
		invalid := evidence("refs/heads/feature", head, "push")
		switch field {
		case "run":
			invalid.CommitSHA = strings.Repeat("b", 40)
		case "job":
			invalid.JobCommitSHA = strings.Repeat("b", 40)
		case "task":
			invalid.TaskCommitSHA = strings.Repeat("b", 40)
		}
		if _, err = verifyBinding(invalid, branch); err == nil {
			t.Fatalf("mismatched %s commit accepted", field)
		}
	}
	pr, err := parse("42", "", head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifyBinding(evidence("refs/pull/42/head", head, "pull_request"), pr); err != nil {
		t.Fatal(err)
	}
	if _, err = verifyBinding(evidence("refs/pull/41/head", head, "pull_request"), pr); err == nil {
		t.Fatal("wrong provider PR accepted")
	}
	if _, err = parse("0", "", head); err == nil {
		t.Fatal("unbound branch/PR accepted")
	}
	duplicate := httptest.NewRequest("GET", "/logs?provider_pr=42&provider_pr=0&head_ref=feature&head_sha="+head, nil)
	if _, err = parseExpectedBinding(duplicate); err == nil {
		t.Fatal("ambiguous duplicate binding accepted")
	}
}

func TestCIBridgeBindsDetachedWorkflowDispatchExactly(t *testing.T) {
	head := strings.Repeat("a", 40)
	expected := expectedBinding{HeadRef: head, HeadSHA: head}
	valid := logBinding{CommitSHA: head, JobCommitSHA: head, TaskCommitSHA: head, ProviderRef: head, Event: "workflow_dispatch"}
	actual, err := verifyBinding(valid, expected)
	if err != nil || actual.ProviderPR != 0 || actual.HeadRef != head || actual.ProviderRef != head || actual.Event != "workflow_dispatch" {
		t.Errorf("exact detached dispatch rejected: %+v %v", actual, err)
	}
	for _, test := range []struct {
		name string
		edit func(*logBinding, *expectedBinding)
	}{
		{"foreign_pr", func(_ *logBinding, e *expectedBinding) { e.ProviderPR = 42 }},
		{"query_ref", func(_ *logBinding, e *expectedBinding) { e.HeadRef = "feature" }},
		{"stored_ref", func(b *logBinding, _ *expectedBinding) { b.ProviderRef = strings.Repeat("b", 40) }},
		{"spaced_ref", func(b *logBinding, _ *expectedBinding) { b.ProviderRef = " " + head }},
		{"push", func(b *logBinding, _ *expectedBinding) { b.Event = "push" }},
		{"missing_event", func(b *logBinding, _ *expectedBinding) { b.Event = "" }},
		{"unknown_event", func(b *logBinding, _ *expectedBinding) { b.Event = "unknown" }},
		{"spaced_event", func(b *logBinding, _ *expectedBinding) { b.Event = "workflow_dispatch " }},
		{"run_commit", func(b *logBinding, _ *expectedBinding) { b.CommitSHA = strings.Repeat("b", 40) }},
		{"job_commit", func(b *logBinding, _ *expectedBinding) { b.JobCommitSHA = strings.Repeat("b", 40) }},
		{"task_commit", func(b *logBinding, _ *expectedBinding) { b.TaskCommitSHA = strings.Repeat("b", 40) }},
		{"noncanonical_commit", func(b *logBinding, e *expectedBinding) {
			upper := strings.Repeat("A", 40)
			b.CommitSHA, b.JobCommitSHA, b.TaskCommitSHA, b.ProviderRef = upper, upper, upper, upper
			e.HeadRef, e.HeadSHA = upper, upper
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding, query := valid, expected
			test.edit(&binding, &query)
			if _, err := verifyBinding(binding, query); err == nil {
				t.Fatal("unbound detached dispatch accepted")
			}
		})
	}
}
