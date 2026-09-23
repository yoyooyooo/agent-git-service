package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

func exactIntentCandidate(t *testing.T, svc *Service, id, key string, pr db.PullRequest, projection db.PullRequestProjection) db.PullRequestActionIntent {
	intent := db.PullRequestActionIntent{
		ID: id, IdempotencyKey: key, Action: "pr.rebase", State: ForgejoActionIntentPlanned,
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AGSPRNumber: pr.Number, Repository: "example-owner/demo",
		ForgejoRepo: projection.ExternalRepo, ForgejoPRNumber: projection.ExternalNumber,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, ExpectedHeadSHA: pr.HeadSHA, ExpectedBaseSHA: pr.BaseSHA,
		ExpectedLabels: "[]", PostLabels: `["ags/action-rebase"]`, PrincipalID: 1,
		ExpiresInSeconds: 300, ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}
	stampDurableActionIntentAuthority(t, svc, &intent)
	return intent
}

func TestRecoveryQuarantinesHistoricalDelegatedDispatchWithoutInventingReceipt(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	sessionID := "11111111-1111-4111-8111-111111111111"
	intent := exactIntentCandidate(t, svc, "historical-delegated-dispatch", "historical-delegated-dispatch", pr, projection)
	intent.State = ForgejoActionIntentDispatching
	intent.AgentSessionID = &sessionID
	intent.ProviderEffectStatus = ""
	intent.BoundaryProtocol = ""
	intent.BoundaryReceiptID = nil
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	providerCalls := client.addCalls
	client.mu.Unlock()
	if intent.State != ForgejoActionIntentRecovery || intent.FailureCode != "boundary_receipt_history_unproven" ||
		intent.ProviderEffectStatus != "" || intent.BoundaryReceiptID != nil || providerCalls != 0 {
		t.Fatalf("historical dispatch was not honestly quarantined: intent=%#v provider_calls=%d", intent, providerCalls)
	}
	receipt, err := actionIntentReceipt(intent)
	if err != nil || receipt.ProviderEffectStatus != ProviderEffectStatusNotRecorded {
		t.Fatalf("historical wire status=%#v err=%v", receipt, err)
	}
}

func TestRecoveryQuarantinesNewProtocolMissingReceiptWithoutProviderCall(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	sessionID := "22222222-2222-4222-8222-222222222222"
	intent := exactIntentCandidate(t, svc, "new-protocol-missing-receipt", "new-protocol-missing-receipt", pr, projection)
	intent.State = ForgejoActionIntentDispatching
	intent.AgentSessionID = &sessionID
	intent.BoundaryProtocol = AuthorityBoundaryReceiptDelegatedEffectKind
	intent.ProviderEffectStatus = ProviderEffectStatusOutcomeUnknown
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	providerCalls := client.addCalls
	client.mu.Unlock()
	if intent.State != ForgejoActionIntentRecovery || intent.FailureCode != "boundary_receipt_integrity_failure" ||
		intent.ProviderEffectStatus != ProviderEffectStatusOutcomeUnknown || providerCalls != 0 {
		t.Fatalf("missing receipt was not quarantined: intent=%#v provider_calls=%d", intent, providerCalls)
	}
}

func TestForgejoActionIntentSHARequestDurableReceiptAndRecoveryRoundTrip(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	client.overrideLabels = true
	client.addErr = errors.New("simulated unknown provider outcome")

	var viewer db.User
	if err := svc.DB.First(&viewer, 1).Error; err != nil {
		t.Fatal(err)
	}
	request := ForgejoActionIntentRequest{
		IdempotencyKey: "sha-roundtrip", Repository: "example-owner/demo",
		AGSPRNumber: pr.Number, ForgejoPRNumber: projection.ExternalNumber,
		ExpectedHeadSHA: pr.HeadSHA, ExpectedBaseSHA: pr.BaseSHA,
		ExpectedLabels: []string{}, PostLabels: []string{forgejointegration.AGSActionRebaseLabel},
		ExpiresIn: 5 * time.Minute,
	}
	receipt, err := svc.RequestForgejoActionRebase(context.Background(), viewer, request)
	if err != nil {
		t.Fatalf("request action intent: %v", err)
	}
	if receipt.State != ForgejoActionIntentDispatching || receipt.ExpectedHeadSHA != request.ExpectedHeadSHA || receipt.ExpectedBaseSHA != request.ExpectedBaseSHA {
		t.Fatalf("request receipt did not preserve exact SHAs: %#v", receipt)
	}

	var persisted db.PullRequestActionIntent
	if err := svc.DB.First(&persisted, "id = ?", receipt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.ExpectedHeadSHA != request.ExpectedHeadSHA || persisted.ExpectedBaseSHA != request.ExpectedBaseSHA ||
		!isValidGitSHA(persisted.ExpectedHeadSHA) || !isValidGitSHA(persisted.ExpectedBaseSHA) {
		t.Fatalf("durable intent did not preserve exact canonical SHAs: %#v", persisted)
	}
	getReceipt, err := svc.GetForgejoActionIntent(context.Background(), viewer, request.Repository, request.AGSPRNumber, receipt.ID)
	if err != nil {
		t.Fatalf("get action receipt: %v", err)
	}
	if getReceipt.ExpectedHeadSHA != request.ExpectedHeadSHA || getReceipt.ExpectedBaseSHA != request.ExpectedBaseSHA {
		t.Fatalf("GET receipt did not preserve exact SHAs: %#v", getReceipt)
	}

	client.mu.Lock()
	client.addErr = nil
	client.mu.Unlock()
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatalf("recover action intent: %v", err)
	}
	recovered, err := svc.GetForgejoActionIntent(context.Background(), viewer, request.Repository, request.AGSPRNumber, receipt.ID)
	if err != nil {
		t.Fatalf("get recovered action receipt: %v", err)
	}
	if recovered.State != ForgejoActionIntentDispatched || recovered.ExpectedHeadSHA != request.ExpectedHeadSHA || recovered.ExpectedBaseSHA != request.ExpectedBaseSHA {
		t.Fatalf("recovery receipt did not preserve exact SHAs: %#v", recovered)
	}
	client.mu.Lock()
	addCalls := client.addCalls
	client.mu.Unlock()
	if addCalls != 2 {
		t.Fatalf("provider dispatch calls=%d, want initial unknown outcome plus one recovery", addCalls)
	}
}

func TestForgejoActionIntentRejectsNonCanonicalSHAsBeforeIntentOrProviderWrite(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	var viewer db.User
	if err := svc.DB.First(&viewer, 1).Error; err != nil {
		t.Fatal(err)
	}
	canonicalHead, canonicalBase := pr.HeadSHA, pr.BaseSHA
	for _, tc := range []struct {
		name string
		head string
		base string
	}{
		{name: "head leading whitespace", head: " " + canonicalHead, base: canonicalBase},
		{name: "head trailing whitespace", head: canonicalHead + " ", base: canonicalBase},
		{name: "head uppercase", head: strings.Repeat("A", 40), base: canonicalBase},
		{name: "head short", head: strings.Repeat("a", 39), base: canonicalBase},
		{name: "head long", head: strings.Repeat("a", 41), base: canonicalBase},
		{name: "head null representation", head: "", base: canonicalBase},
		{name: "base leading whitespace", head: canonicalHead, base: " " + canonicalBase},
		{name: "base trailing whitespace", head: canonicalHead, base: canonicalBase + " "},
		{name: "base uppercase", head: canonicalHead, base: strings.Repeat("B", 40)},
		{name: "base short", head: canonicalHead, base: strings.Repeat("b", 39)},
		{name: "base long", head: canonicalHead, base: strings.Repeat("b", 41)},
		{name: "base null representation", head: canonicalHead, base: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.RequestForgejoActionRebase(context.Background(), viewer, ForgejoActionIntentRequest{
				IdempotencyKey: "invalid-sha-" + tc.name, Repository: "example-owner/demo",
				AGSPRNumber: pr.Number, ForgejoPRNumber: projection.ExternalNumber,
				ExpectedHeadSHA: tc.head, ExpectedBaseSHA: tc.base,
				ExpectedLabels: []string{}, PostLabels: []string{forgejointegration.AGSActionRebaseLabel},
			})
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("non-canonical SHA error=%v", err)
			}
		})
	}
	var intents int64
	if err := svc.DB.Model(&db.PullRequestActionIntent{}).Count(&intents).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	listCalls, addCalls, removeCalls, commentCalls := client.listCalls, client.addCalls, client.removeCalls, client.commentCalls
	client.mu.Unlock()
	if intents != 0 || listCalls != 0 || addCalls != 0 || removeCalls != 0 || commentCalls != 0 {
		t.Fatalf("invalid SHA crossed validation boundary: intents=%d list=%d add=%d remove=%d comment=%d", intents, listCalls, addCalls, removeCalls, commentCalls)
	}
}

func TestRebaseConstraintNumbersUseJSONSafePositiveRange(t *testing.T) {
	for _, value := range []json.Number{"1", "9007199254740991"} {
		if !positiveConstraintNumber(value) {
			t.Fatalf("safe positive number %s rejected", value)
		}
	}
	for _, value := range []json.Number{"0", "-1", "9007199254740992"} {
		if positiveConstraintNumber(value) {
			t.Fatalf("unsafe number %s accepted", value)
		}
	}
	constraints := forgejoRebaseSessionConstraints(1, 2, strings.Repeat("a", 40), strings.Repeat("b", 40))
	constraints["pull_request_number"] = "9007199254740992"
	if delegatedOperationConstraintsValid("pr.rebase", constraints) {
		t.Fatal("delegated Session accepted an unsafe PR number")
	}
}

func TestSameForgejoActionIntentFactsRejectsEveryExternalCoordinateChange(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	base := exactIntentCandidate(t, svc, "intent-a", "same-key", pr, projection)
	for name, mutate := range map[string]func(*db.PullRequestActionIntent){
		"forgejo repository": func(v *db.PullRequestActionIntent) { v.ForgejoRepo = "forgejo/other" },
		"forgejo PR":         func(v *db.PullRequestActionIntent) { v.ForgejoPRNumber++ },
		"AGS repository":     func(v *db.PullRequestActionIntent) { v.Repository = "example-owner/other" },
		"AGS PR":             func(v *db.PullRequestActionIntent) { v.AGSPRNumber++ },
		"expected head":      func(v *db.PullRequestActionIntent) { v.ExpectedHeadSHA = strings.Repeat("a", 40) },
		"expected base":      func(v *db.PullRequestActionIntent) { v.ExpectedBaseSHA = strings.Repeat("b", 40) },
		"pre labels":         func(v *db.PullRequestActionIntent) { v.ExpectedLabels = `["reviewed"]` },
		"post labels":        func(v *db.PullRequestActionIntent) { v.PostLabels = `["reviewed"]` },
		"principal":          func(v *db.PullRequestActionIntent) { v.PrincipalID++ },
		"team":               func(v *db.PullRequestActionIntent) { v.TeamIdentityID = "team-b" },
		"policy class":       func(v *db.PullRequestActionIntent) { v.PolicyClass = "class-b" },
		"membership epoch":   func(v *db.PullRequestActionIntent) { v.MembershipEpoch++ },
		"authority revision": func(v *db.PullRequestActionIntent) { v.AuthorityRev = "revision-b" },
		"expiry":             func(v *db.PullRequestActionIntent) { v.ExpiresInSeconds++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if sameForgejoActionIntentFacts(base, changed) {
				t.Fatal("changed exact fact was accepted as an idempotent retry")
			}
		})
	}
}

func TestCommitForgejoActionIntentSameKeyRejectsChangedForgejoCoordinates(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	original := exactIntentCandidate(t, svc, "intent-idempotent", "key-idempotent", pr, projection)
	if err := svc.commitForgejoActionIntent(context.Background(), &original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		repo   string
		number int
	}{
		{name: "repository", repo: "forgejo/other", number: projection.ExternalNumber},
		{name: "pull request", repo: projection.ExternalRepo, number: projection.ExternalNumber + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := svc.DB.Model(&db.PullRequestProjection{}).Where("id = ?", projection.ID).
				Updates(map[string]any{"external_repo": tc.repo, "external_number": tc.number}).Error; err != nil {
				t.Fatal(err)
			}
			changed := exactIntentCandidate(t, svc, "intent-idempotent-changed-"+tc.name, original.IdempotencyKey, pr, projection)
			changed.ForgejoRepo, changed.ForgejoPRNumber = tc.repo, tc.number
			if err := svc.commitForgejoActionIntent(context.Background(), &changed); !errors.Is(err, ErrForbidden) {
				t.Fatalf("changed coordinate was not rejected: %v", err)
			}
			if err := svc.DB.Model(&db.PullRequestProjection{}).Where("id = ?", projection.ID).
				Updates(map[string]any{"external_repo": projection.ExternalRepo, "external_number": projection.ExternalNumber}).Error; err != nil {
				t.Fatal(err)
			}
		})
	}
	var count int64
	if err := svc.DB.Model(&db.PullRequestActionIntent{}).Where("idempotency_key = ?", original.IdempotencyKey).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("idempotent rows=%d err=%v", count, err)
	}
}

func TestCommitForgejoActionIntentExpiresOldBeforeCreatingNew(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	old := exactIntentCandidate(t, svc, "intent-expired", "key-expired", pr, projection)
	old.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := svc.DB.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	fresh := exactIntentCandidate(t, svc, "intent-fresh", "key-fresh", pr, projection)
	if err := svc.commitForgejoActionIntent(context.Background(), &fresh); err != nil {
		t.Fatalf("commit fresh intent: %v", err)
	}
	if err := svc.DB.First(&old, "id = ?", old.ID).Error; err != nil {
		t.Fatal(err)
	}
	if old.State != ForgejoActionIntentDenied || old.FailureCode != "expired" {
		t.Fatalf("expired intent=%#v", old)
	}
}

func TestCommitForgejoActionIntentConcurrentCallersLeaveOneActive(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	sqlDB, err := svc.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(4)
	intents := []db.PullRequestActionIntent{
		exactIntentCandidate(t, svc, "intent-concurrent-a", "key-concurrent-a", pr, projection),
		exactIntentCandidate(t, svc, "intent-concurrent-b", "key-concurrent-b", pr, projection),
	}
	start := make(chan struct{})
	results := make(chan error, len(intents))
	var wg sync.WaitGroup
	for index := range intents {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results <- svc.commitForgejoActionIntent(context.Background(), &intents[index])
		}(index)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	var active int64
	if err := svc.DB.Model(&db.PullRequestActionIntent{}).
		Where("pull_request_id = ? AND state IN ?", pr.ID, forgejoActionActiveStates()).Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	if successes != 1 || active != 1 {
		t.Fatalf("successes=%d active=%d, want exactly one", successes, active)
	}
}

func TestConsumeForgejoActionIntentDuplicateActiveFailsClosed(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	for _, suffix := range []string{"a", "b"} {
		intent := exactIntentCandidate(t, svc, "intent-duplicate-"+suffix, "key-duplicate-"+suffix, pr, projection)
		intent.State = ForgejoActionIntentDispatched
		if err := svc.DB.Create(&intent).Error; err != nil {
			t.Fatal(err)
		}
	}
	_, accepted, denial, err := svc.consumeForgejoActionIntent(context.Background(), forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
		HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
	})
	if err != nil || accepted || denial != forgejoActionIntentDenialFactMismatch {
		t.Fatalf("accepted=%v denial=%q err=%v", accepted, denial, err)
	}
	var active int64
	if err := svc.DB.Model(&db.PullRequestActionIntent{}).Where("pull_request_id = ? AND state = ?", pr.ID, ForgejoActionIntentDispatched).Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	listCalls := client.listCalls
	client.mu.Unlock()
	if active != 0 || listCalls != 0 {
		t.Fatalf("active=%d provider reads=%d, want fail closed before provider", active, listCalls)
	}
}

func TestExactActionLiveDriftDeniesBeforeProviderMutation(t *testing.T) {
	for name, drift := range map[string]func(*Service, db.PullRequest, *minimalForgejoClient){
		"AGS head": func(svc *Service, pr db.PullRequest, _ *minimalForgejoClient) {
			if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("head_sha", strings.Repeat("a", 40)).Error; err != nil {
				t.Fatal(err)
			}
		},
		"Forgejo head": func(_ *Service, pr db.PullRequest, client *minimalForgejoClient) {
			client.mu.Lock()
			defer client.mu.Unlock()
			client.pr.HeadSHA = strings.Repeat("b", 40)
			client.remoteRefs[pr.HeadRef] = strings.Repeat("b", 40)
		},
		"Forgejo base": func(_ *Service, pr db.PullRequest, client *minimalForgejoClient) {
			client.mu.Lock()
			defer client.mu.Unlock()
			client.remoteRefs[pr.BaseRef] = strings.Repeat("c", 40)
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, pr, projection, client := setupIntentTestServiceWithClient(t)
			intent := seedDispatchedIntent(t, svc, pr, projection)
			drift(svc, pr, client)
			ctx := contextWithForgejoActionIntent(context.Background(), intent.ID)
			err := svc.addForgejoPullRequestLabels(ctx, projection.ExternalRepo, projection.ExternalNumber, []string{"ags/status-rebasing"})
			if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
				t.Fatalf("drift was not denied: %v", err)
			}
			client.mu.Lock()
			addCalls := client.addCalls
			client.mu.Unlock()
			if addCalls != 0 {
				t.Fatalf("drift reached provider: add calls=%d", addCalls)
			}
			if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			if intent.State != ForgejoActionIntentDenied || intent.FailureCode != "exact_action_fact_drift" {
				t.Fatalf("intent=%#v", intent)
			}
		})
	}
}

func TestWebhookLiveDriftTerminalizesBeforeTriggerOrRebaseMutation(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	client.mu.Lock()
	client.pr.HeadSHA = strings.Repeat("e", 40)
	client.remoteRefs[pr.HeadRef] = strings.Repeat("e", 40)
	client.mu.Unlock()
	result, err := svc.handleForgejoPullRequestActionLabel(context.Background(), forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
		HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef, LabelName: forgejointegration.AGSActionRebaseLabel,
	})
	if err != nil || result.WorkflowStatus != "denied" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	client.mu.Lock()
	addCalls, removeCalls, commentCalls := client.addCalls, client.removeCalls, client.commentCalls
	client.mu.Unlock()
	if addCalls == 0 || removeCalls == 0 || commentCalls == 0 {
		t.Fatalf("head drift must still strip the action label and publish status: add=%d remove=%d comment=%d", addCalls, removeCalls, commentCalls)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || intent.FailureCode != "exact_action_fact_drift" {
		t.Fatalf("intent=%#v", intent)
	}
}

func TestActionRebaseAllowsFastForwardedBaseWhenHeadIsStable(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	ctx := context.Background()
	newBase, err := svc.Git.WriteFile(ctx, "example-owner/demo", "main", "forward.txt", "move main", []byte("forward\n"))
	if err != nil {
		t.Fatalf("advance main: %v", err)
	}
	client.mu.Lock()
	client.remoteRefs[pr.BaseRef] = newBase
	client.mu.Unlock()
	preflight, err := svc.preflightForgejoActionRebase(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
		HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
	}, pr, intent)
	if err != nil {
		t.Fatalf("fast-forwarded base should be rebase-eligible: %v", err)
	}
	if !exactGitSHA(preflight.BaseSHA, newBase) || !exactGitSHA(preflight.HeadSHA, pr.HeadSHA) {
		t.Fatalf("preflight=%#v want live base %s and stable head %s", preflight, newBase, pr.HeadSHA)
	}
}

func TestDurableActionIntentGETRequiresOriginalPrincipalAndFreshExactRebaseSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drift func(*testing.T, *Service, db.PullRequestActionIntent) db.User
	}{
		{name: "original principal succeeds", drift: func(t *testing.T, svc *Service, intent db.PullRequestActionIntent) db.User {
			var viewer db.User
			if err := svc.DB.First(&viewer, intent.PrincipalID).Error; err != nil {
				t.Fatal(err)
			}
			return viewer
		}},
		{name: "other principal denied", drift: func(t *testing.T, svc *Service, _ db.PullRequestActionIntent) db.User {
			viewer := db.User{Login: "other-viewer", Type: db.TypeUser, Status: db.UserStatusActive}
			if err := svc.DB.Create(&viewer).Error; err != nil {
				t.Fatal(err)
			}
			return viewer
		}},
		{name: "pr.read only denied", drift: func(t *testing.T, svc *Service, intent db.PullRequestActionIntent) db.User {
			svc.PrincipalSessions = durableActionTestAuthority(t, intent.PrincipalID, "read-v1", []string{"pr.read", "repo.read"})
			var viewer db.User
			if err := svc.DB.First(&viewer, intent.PrincipalID).Error; err != nil {
				t.Fatal(err)
			}
			return viewer
		}},
		{name: "policy revision drift denied", drift: func(t *testing.T, svc *Service, intent db.PullRequestActionIntent) db.User {
			svc.PrincipalSessions = durableActionTestAuthority(t, intent.PrincipalID, "class-v2", nil)
			var viewer db.User
			if err := svc.DB.First(&viewer, intent.PrincipalID).Error; err != nil {
				t.Fatal(err)
			}
			return viewer
		}},
		{name: "native grant revoke denied", drift: func(t *testing.T, svc *Service, intent db.PullRequestActionIntent) db.User {
			newOwner := db.User{Login: "replacement-owner", Type: db.TypeUser, Status: db.UserStatusActive}
			if err := svc.DB.Create(&newOwner).Error; err != nil {
				t.Fatal(err)
			}
			if err := svc.DB.Model(&db.Repository{}).Where("id = ?", intent.RepositoryID).Update("owner_id", newOwner.ID).Error; err != nil {
				t.Fatal(err)
			}
			var viewer db.User
			if err := svc.DB.First(&viewer, intent.PrincipalID).Error; err != nil {
				t.Fatal(err)
			}
			return viewer
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, pr, projection := setupIntentTestService(t)
			intent := seedDispatchedIntent(t, svc, pr, projection)
			viewer := tc.drift(t, svc, intent)
			receipt, err := svc.GetForgejoActionIntent(context.Background(), viewer, intent.Repository, intent.AGSPRNumber, intent.ID)
			if tc.name == "original principal succeeds" {
				if err != nil || receipt.ID != intent.ID {
					t.Fatalf("receipt=%#v err=%v", receipt, err)
				}
				return
			}
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
		})
	}
}

func TestDurableAuthorityDriftDeniesEveryProviderWriteSeam(t *testing.T) {
	for _, seam := range []struct {
		name  string
		call  func(*Service, context.Context, db.PullRequestActionIntent) error
		calls func(*minimalForgejoClient) int
	}{
		{name: "add labels", call: func(s *Service, ctx context.Context, intent db.PullRequestActionIntent) error {
			return s.addForgejoPullRequestLabels(ctx, intent.ForgejoRepo, intent.ForgejoPRNumber, []string{"status"})
		}, calls: func(c *minimalForgejoClient) int { return c.addCalls }},
		{name: "remove label", call: func(s *Service, ctx context.Context, intent db.PullRequestActionIntent) error {
			return s.removeForgejoPullRequestLabel(ctx, intent.ForgejoRepo, intent.ForgejoPRNumber, "status")
		}, calls: func(c *minimalForgejoClient) int { return c.removeCalls }},
		{name: "create comment", call: func(s *Service, ctx context.Context, intent db.PullRequestActionIntent) error {
			return s.createForgejoPullRequestComment(ctx, intent.ForgejoRepo, intent.ForgejoPRNumber, "status")
		}, calls: func(c *minimalForgejoClient) int { return c.commentCalls }},
	} {
		t.Run(seam.name, func(t *testing.T) {
			svc, pr, projection, client := setupIntentTestServiceWithClient(t)
			intent := seedDispatchedIntent(t, svc, pr, projection)
			svc.PrincipalSessions = durableActionTestAuthority(t, intent.PrincipalID, "class-v2", nil)
			err := seam.call(svc, contextWithForgejoActionIntent(context.Background(), intent.ID), intent)
			if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
				t.Fatalf("provider seam error=%v", err)
			}
			if calls := seam.calls(client); calls != 0 {
				t.Fatalf("denied provider calls=%d", calls)
			}
			if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			if intent.State != ForgejoActionIntentDenied || intent.FailureCode != DelegatedDenialAuthoritySnapshotChanged {
				t.Fatalf("intent=%#v", intent)
			}
		})
	}
}

func TestStartupRecoveryMissingDurableAuthoritySnapshotDeniesWithoutProviderCall(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := exactIntentCandidate(t, svc, "intent-recovery-missing-authority", "key-recovery-missing-authority", pr, projection)
	intent.State = ForgejoActionIntentDispatched
	intent.TeamIdentityID = ""
	intent.PolicyClass = ""
	intent.MembershipEpoch = 0
	intent.AuthorityRev = ""
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || intent.FailureCode != DelegatedDenialAuthoritySnapshotChanged || client.addCalls != 0 {
		t.Fatalf("intent=%#v provider calls=%d", intent, client.addCalls)
	}
}

func TestStartupRecoveryDurablePolicyDriftTerminalizesWithoutProviderCall(t *testing.T) {
	for _, state := range []string{ForgejoActionIntentPlanned, ForgejoActionIntentDispatched, ForgejoActionIntentRunning, ForgejoActionIntentRecovery} {
		t.Run(state, func(t *testing.T) {
			svc, pr, projection, client := setupIntentTestServiceWithClient(t)
			intent := exactIntentCandidate(t, svc, "intent-recovery-authority-drift", "key-recovery-authority-drift", pr, projection)
			intent.State = state
			if err := svc.DB.Create(&intent).Error; err != nil {
				t.Fatal(err)
			}
			svc.PrincipalSessions = durableActionTestAuthority(t, intent.PrincipalID, "class-v2", nil)
			if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			if intent.State != ForgejoActionIntentDenied || intent.FailureCode != DelegatedDenialAuthoritySnapshotChanged || client.addCalls != 0 {
				t.Fatalf("intent=%#v provider calls=%d", intent, client.addCalls)
			}
		})
	}
}

func TestDurableNativeGrantRevokeDeniesProviderBeforeWrite(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	newOwner := db.User{Login: "grant-revoke-owner", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := svc.DB.Create(&newOwner).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.Repository{}).Where("id = ?", intent.RepositoryID).Update("owner_id", newOwner.ID).Error; err != nil {
		t.Fatal(err)
	}
	err := svc.addForgejoPullRequestLabels(contextWithForgejoActionIntent(context.Background(), intent.ID), intent.ForgejoRepo, intent.ForgejoPRNumber, []string{"status"})
	if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) || client.addCalls != 0 {
		t.Fatalf("error=%v provider calls=%d", err, client.addCalls)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied {
		t.Fatalf("intent=%#v", intent)
	}
}

func TestRecoverPlannedIntentWithLiveDriftMakesNoProviderMutation(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := exactIntentCandidate(t, svc, "intent-recovery-drift", "key-recovery-drift", pr, projection)
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.remoteRefs[pr.BaseRef] = strings.Repeat("d", 40)
	client.mu.Unlock()
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	addCalls := client.addCalls
	client.mu.Unlock()
	if intent.State != ForgejoActionIntentDenied || addCalls != 0 {
		t.Fatalf("intent state=%s provider calls=%d", intent.State, addCalls)
	}
}
