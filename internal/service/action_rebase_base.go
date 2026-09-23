package service

import (
	"context"
	"fmt"
	"strings"
)

// agreeLiveRebaseBase decides whether live AGS/Forgejo base refs may be used as
// the rebase target. Head-stable + base fast-forward is the reason action-rebase
// exists; a rewritten or disagreeing base is still drift.
func agreeLiveRebaseBase(ctx context.Context, repoPath, expectedBase, liveAGS, liveForgejo string) (string, error) {
	expectedBase = strings.TrimSpace(expectedBase)
	liveAGS = strings.TrimSpace(liveAGS)
	liveForgejo = strings.TrimSpace(liveForgejo)
	if expectedBase == "" || liveAGS == "" || liveForgejo == "" {
		return "", fmt.Errorf("live AGS and Forgejo base facts are incomplete")
	}
	if !exactGitSHA(liveAGS, liveForgejo) {
		return "", fmt.Errorf("live AGS base %s disagrees with Forgejo base %s", liveAGS, liveForgejo)
	}
	if exactGitSHA(liveAGS, expectedBase) {
		return liveAGS, nil
	}
	if repoPath == "" {
		return "", fmt.Errorf("cannot prove base fast-forward without a repo path")
	}
	forward, err := gitCommitIsAncestor(ctx, repoPath, expectedBase, liveAGS)
	if err != nil {
		return "", fmt.Errorf("prove base fast-forward: %w", err)
	}
	if !forward {
		return "", fmt.Errorf("live base %s is not a fast-forward of intent base %s", liveAGS, expectedBase)
	}
	return liveAGS, nil
}
