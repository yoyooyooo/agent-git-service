package forgejointegration

import "testing"

func TestResolveMergedPullRequestRepoAllowsMappedAutoPRBranch(t *testing.T) {
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "token",
		AutoPullRequest:      true,
		DefaultBaseBranch:    "main",
		MirrorBranchIncludes: []string{"agent/*", "feature/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]RepoMapping{
			"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"},
		},
	}, nil, nil)

	repo, ok := integration.ResolveMergedPullRequestRepo(MergedPullRequestEvent{
		RepoFullName: "forgejo/demo",
		PRNumber:     7,
		HeadBranch:   "agent/demo",
		BaseBranch:   "main",
	})
	if !ok || repo != "example-owner/demo" {
		t.Fatalf("repo=%q ok=%v", repo, ok)
	}
}

func TestResolveMergedPullRequestRepoRejectsUnmappedOrDisallowedBranch(t *testing.T) {
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "token",
		AutoPullRequest:      true,
		DefaultBaseBranch:    "main",
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]RepoMapping{
			"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"},
		},
	}, nil, nil)

	cases := []MergedPullRequestEvent{
		{RepoFullName: "other/demo", HeadBranch: "agent/demo", BaseBranch: "main"},
		{RepoFullName: "forgejo/demo", HeadBranch: "main", BaseBranch: "main"},
		{RepoFullName: "forgejo/demo", HeadBranch: "feature/demo", BaseBranch: "main"},
		{RepoFullName: "forgejo/demo", HeadBranch: "agent/demo", BaseBranch: "release"},
	}
	for _, tc := range cases {
		if repo, ok := integration.ResolveMergedPullRequestRepo(tc); ok {
			t.Fatalf("unexpected repo=%q for %#v", repo, tc)
		}
	}
}
