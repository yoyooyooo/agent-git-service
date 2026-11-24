package service

import (
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

func TestCanonicalProviderMergeSnapshotRequiresMergedExpectedHead(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name     string
		snapshot forgejointegration.PullRequestSnapshot
		want     bool
	}{
		{name: "merged expected head", snapshot: forgejointegration.PullRequestSnapshot{Merged: true, HeadSHA: head, MergeCommitSHA: head}, want: true},
		{name: "merged wrong head", snapshot: forgejointegration.PullRequestSnapshot{Merged: true, HeadSHA: strings.Repeat("b", 40), MergeCommitSHA: head}},
		{name: "merged missing head", snapshot: forgejointegration.PullRequestSnapshot{Merged: true, MergeCommitSHA: head}},
		{name: "unmerged with merge SHA", snapshot: forgejointegration.PullRequestSnapshot{HeadSHA: head, MergeCommitSHA: head}},
		{name: "unmerged at expected base", snapshot: forgejointegration.PullRequestSnapshot{HeadSHA: head, BaseSHA: head}},
		{name: "merged with exact base fallback", snapshot: forgejointegration.PullRequestSnapshot{Merged: true, HeadSHA: head, BaseSHA: head}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sha, ok := canonicalProviderMergeSnapshotSHA(tc.snapshot, head)
			if ok != tc.want || (ok && sha != head) || (!ok && sha != "") {
				t.Fatalf("sha=%q confirmed=%v, want confirmed=%v", sha, ok, tc.want)
			}
		})
	}
}
