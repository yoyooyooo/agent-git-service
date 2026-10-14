package forgejointegration

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// VerifyRepositoryAuthorityReadiness observes every configured repository using
// at most four provider requests concurrently. It retains no authority cache.
// Mutation/merge preflights continue to use their existing exact read paths.
func (i *Integration) VerifyRepositoryAuthorityReadiness(ctx context.Context) ([]RepositoryAuthorityPlan, error) {
	if err := i.AuthorityPolicyConfigured(); err != nil {
		return nil, err
	}
	targets, err := i.authorityPolicyTargets()
	if err != nil {
		return nil, err
	}
	plans := make([]RepositoryAuthorityPlan, len(targets))
	errors := make([]error, len(targets))
	next := make(chan int)
	workers := min(4, len(targets))
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for index := range next {
				target := targets[index]
				if err := ctx.Err(); err != nil {
					errors[index] = err
					continue
				}
				state, err := i.authorityClient.InspectRepositoryAuthority(ctx, target.owner, target.repo, target.baseBranch, i.cfg.WebhookURL, i.cfg.IntegrationBot)
				if err != nil {
					errors[index] = fmt.Errorf("inspect Forgejo authority for %s: %w", target.externalRepo(), err)
					continue
				}
				changes := authorityPolicyChanges(state)
				plans[index] = RepositoryAuthorityPlan{AGSRepo: target.agsRepo, ForgejoRepo: target.externalRepo(), BaseBranch: target.baseBranch, Changes: changes, Converged: len(changes) == 0}
				if len(changes) > 0 {
					errors[index] = fmt.Errorf("Forgejo authority policy drift for %s: %s", target.externalRepo(), strings.Join(changes, ", "))
				}
			}
		})
	}
	for index := range targets {
		next <- index
	}
	close(next)
	group.Wait()
	for _, err := range errors {
		if err != nil {
			return plans, err
		}
	}
	return plans, nil
}
