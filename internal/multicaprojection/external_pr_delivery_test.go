package multicaprojection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func terminalRequest(state, mergedSHA string) ExternalPRLinkRequest {
	return ExternalPRLinkRequest{
		Provider:         "ags",
		IssueID:          "issue-id",
		WorkspaceID:      "workspace-id",
		Workspace:        "workspace-alpha",
		IssueKey:         "HUM-42",
		ExternalRepo:     "owner/repo",
		ExternalNumber:   7,
		ExternalURL:      "https://ags.example/owner/repo/pull/7",
		MergeProvider:    "forgejo",
		MergeRepo:        "forgejo/repo",
		MergeNumber:      42,
		MergeURL:         "https://forgejo.example/forgejo/repo/pulls/42",
		MergedSHA:        mergedSHA,
		CompletionIntent: state == "merged",
		LinkConfidence:   "authoritative",
		State:            state,
	}
}

func TestExternalPRTerminalDeliveryUsesCanonicalIdentityAndExactWireRequest(t *testing.T) {
	observed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	input := ExternalPRTerminalDeliveryInput{
		TargetInstance: "mini-prod",
		Request:        terminalRequest("merged", strings.Repeat("b", 40)),
		AGSPrID:        "owner/repo#7",
		ObservedAt:     observed,
	}
	first, err := NewExternalPRTerminalDelivery(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewExternalPRTerminalDelivery(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.IdempotencyKey == "" || first.IdempotencyKey != second.IdempotencyKey || first.DeliveryID != second.DeliveryID {
		t.Fatalf("delivery identity is not stable: first=%#v second=%#v", first, second)
	}
	if first.Request.Provider != "ags" || first.Request.ExternalRepo != "owner/repo" || first.Request.ExternalNumber != 7 || first.Request.MergeProvider != "forgejo" {
		t.Fatalf("wire identity split AGS and Forgejo facts: %#v", first.Request)
	}
	payload, err := MarshalExternalPRTerminalDelivery(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"service-token", "Bearer", "Authorization"} {
		if strings.Contains(payload, secret) {
			t.Fatalf("secret-shaped value leaked into payload: %q", secret)
		}
	}
	decoded, err := DecodeExternalPRTerminalDelivery([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := MarshalExternalPRLinkRequest(decoded.Request)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := json.Marshal(decoded.Request)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != string(exact) || strings.Contains(string(wire), `"schema"`) || strings.Contains(string(wire), `"target"`) {
		t.Fatalf("wire is not exact closed ExternalPRLinkRequest JSON: %s", wire)
	}
}

func TestExternalPRTerminalDeliveryRequiresAuthoritativeForgejoFactsAndTarget(t *testing.T) {
	badConfidence := terminalRequest("merged", strings.Repeat("b", 40))
	badConfidence.LinkConfidence = "inferred"
	if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{TargetInstance: "mini-prod", Request: badConfidence, AGSPrID: "owner/repo#7"}); err == nil {
		t.Fatal("inferred link confidence was accepted")
	}
	badMergeProvider := terminalRequest("merged", strings.Repeat("b", 40))
	badMergeProvider.MergeProvider = "gitlab"
	if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{TargetInstance: "mini-prod", Request: badMergeProvider, AGSPrID: "owner/repo#7"}); err == nil {
		t.Fatal("non-Forgejo merge provider was accepted")
	}
	if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{Request: terminalRequest("merged", strings.Repeat("b", 40)), AGSPrID: "owner/repo#7"}); err == nil {
		t.Fatal("empty target instance was accepted")
	}
}

func TestExternalPRTerminalDeliveryRejectsProviderAndPartialBinding(t *testing.T) {
	badProvider := terminalRequest("merged", strings.Repeat("b", 40))
	badProvider.Provider = "forgejo"
	if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{TargetInstance: "mini-prod", Request: badProvider, AGSPrID: "owner/repo#7"}); err == nil {
		t.Fatal("expected non-AGS canonical provider rejection")
	}
	partial := terminalRequest("merged", strings.Repeat("b", 40))
	partial.TargetInstance = "mini-prod"
	partial.ProviderBindingID = "binding-1"
	if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{TargetInstance: "mini-prod", Request: partial, AGSPrID: "owner/repo#7"}); err == nil || !strings.Contains(err.Error(), "complete or omitted") {
		t.Fatalf("expected all-or-none binding rejection, got %v", err)
	}
}

func TestExternalPRTerminalDeliveryRejectsInvalidTerminalFacts(t *testing.T) {
	cases := []ExternalPRLinkRequest{
		terminalRequest("merged", ""),
		terminalRequest("closed", strings.Repeat("b", 40)),
		terminalRequest("open", ""),
	}
	for i, request := range cases {
		if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{TargetInstance: "mini-prod", Request: request, AGSPrID: "owner/repo#7"}); err == nil {
			t.Fatalf("case %d unexpectedly accepted: %#v", i, request)
		}
	}
}

func TestExternalPRTerminalDeliveryRejectsURLUserinfoQueryAndFragment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		field func(*ExternalPRLinkRequest, string)
	}{
		{name: "external userinfo", raw: "https://user:pass@ags.example/owner/repo/pull/7", field: func(request *ExternalPRLinkRequest, raw string) { request.ExternalURL = raw }},
		{name: "external query", raw: "https://ags.example/owner/repo/pull/7?token=secret", field: func(request *ExternalPRLinkRequest, raw string) { request.ExternalURL = raw }},
		{name: "external fragment", raw: "https://ags.example/owner/repo/pull/7#details", field: func(request *ExternalPRLinkRequest, raw string) { request.ExternalURL = raw }},
		{name: "external empty fragment", raw: "https://ags.example/owner/repo/pull/7#", field: func(request *ExternalPRLinkRequest, raw string) { request.ExternalURL = raw }},
		{name: "merge userinfo", raw: "https://user:pass@forgejo.example/forgejo/repo/pulls/42", field: func(request *ExternalPRLinkRequest, raw string) { request.MergeURL = raw }},
		{name: "merge query", raw: "https://forgejo.example/forgejo/repo/pulls/42?token=secret", field: func(request *ExternalPRLinkRequest, raw string) { request.MergeURL = raw }},
		{name: "merge fragment", raw: "https://forgejo.example/forgejo/repo/pulls/42#details", field: func(request *ExternalPRLinkRequest, raw string) { request.MergeURL = raw }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := terminalRequest("merged", strings.Repeat("b", 40))
			tc.field(&request, tc.raw)
			if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{TargetInstance: "mini-prod", Request: request, AGSPrID: "owner/repo#7"}); err == nil {
				t.Fatalf("URL %q was accepted", tc.raw)
			}
		})
	}
}

func TestExternalPRTerminalDeliveryRequiresCanonicalMergedSHA(t *testing.T) {
	cases := []struct {
		name    string
		state   string
		sha     string
		wantErr bool
	}{
		{name: "valid lower 40", state: "merged", sha: strings.Repeat("b", 40)},
		{name: "uppercase 40", state: "merged", sha: strings.ToUpper(strings.Repeat("b", 40)), wantErr: true},
		{name: "short 39", state: "merged", sha: strings.Repeat("b", 39), wantErr: true},
		{name: "long 64", state: "merged", sha: strings.Repeat("b", 64), wantErr: true},
		{name: "closed empty", state: "closed", sha: ""},
		{name: "closed with SHA", state: "closed", sha: strings.Repeat("b", 40), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := terminalRequest(tc.state, tc.sha)
			if err := validateExternalPRLinkRequest(request, "mini-prod", false); (err != nil) != tc.wantErr {
				t.Fatalf("validate error = %v, wantErr=%v", err, tc.wantErr)
			}
			_, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{
				TargetInstance: "mini-prod",
				Request:        request,
				AGSPrID:        "owner/repo#7",
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("constructor error = %v, wantErr=%v", err, tc.wantErr)
			}
			wireRequest := request
			wireRequest.IdempotencyKey = "test-key"
			_, err = MarshalExternalPRLinkRequest(wireRequest)
			if (err != nil) != tc.wantErr {
				t.Fatalf("wire validation error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestExternalPRTerminalDeliveryRequiresCompletionIntentForMergedOnly(t *testing.T) {
	mergedWithoutIntent := terminalRequest("merged", strings.Repeat("b", 40))
	mergedWithoutIntent.CompletionIntent = false
	if err := validateExternalPRLinkRequest(mergedWithoutIntent, "mini-prod", false); err == nil || !strings.Contains(err.Error(), "completion intent") {
		t.Fatalf("merged request without completion intent was accepted: %v", err)
	}
	if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{
		TargetInstance: "mini-prod",
		Request:        mergedWithoutIntent,
		AGSPrID:        "owner/repo#7",
	}); err == nil {
		t.Fatal("constructor accepted merged request without completion intent")
	}

	closedWithIntent := terminalRequest("closed", "")
	closedWithIntent.CompletionIntent = true
	if err := validateExternalPRLinkRequest(closedWithIntent, "mini-prod", false); err == nil || !strings.Contains(err.Error(), "cannot claim completion") {
		t.Fatalf("closed request with completion intent was accepted: %v", err)
	}
	if _, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{
		TargetInstance: "mini-prod",
		Request:        closedWithIntent,
		AGSPrID:        "owner/repo#7",
	}); err == nil {
		t.Fatal("constructor accepted closed request with completion intent")
	}
}

func TestDecodeExternalPRTerminalDeliveryRejectsNonCanonicalMergedSHA(t *testing.T) {
	base, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{
		TargetInstance: "mini-prod",
		Request:        terminalRequest("merged", strings.Repeat("b", 40)),
		AGSPrID:        "owner/repo#7",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{
		strings.ToUpper(strings.Repeat("b", 40)),
		strings.Repeat("b", 39),
		strings.Repeat("b", 64),
	} {
		delivery := base
		delivery.Request.MergedSHA = sha
		digest, err := externalPRTerminalIdentityDigest(delivery.Target.InstanceID, delivery.Request)
		if err != nil {
			t.Fatal(err)
		}
		delivery.IdempotencyKey = "external-pr-terminal:v1:" + hex.EncodeToString(digest[:])
		delivery.Request.IdempotencyKey = delivery.IdempotencyKey
		delivery.DeliveryID = deterministicUUID(digest[:])
		payload, err := json.Marshal(delivery)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeExternalPRTerminalDelivery(payload); err == nil {
			t.Fatalf("Decode accepted non-canonical merged SHA %q", sha)
		}
	}
}

func TestCanonicalTerminalRequestFixtureRejectsMissingCompletionIntent(t *testing.T) {
	data, err := os.ReadFile("../../testdata/multica/external-pr-terminal-request.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var request ExternalPRLinkRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	request.CompletionIntent = false
	if _, err := MarshalExternalPRLinkRequest(request); err == nil || !strings.Contains(err.Error(), "completion intent") {
		t.Fatalf("canonical merged fixture without completion intent was accepted: %v", err)
	}
}

func TestCanonicalTerminalRequestFixtureIsClosedAndSecretSafe(t *testing.T) {
	data, err := os.ReadFile("../../testdata/multica/external-pr-terminal-request.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var request ExternalPRLinkRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	wire, err := MarshalExternalPRLinkRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	// The HTTP marshal is the exact JSON bytes without a line ending; the
	// checked-in fixture intentionally carries exactly one trailing LF so its
	// attested file bytes are stable and human-readable.
	if !strings.HasSuffix(string(data), "\n") || strings.HasSuffix(string(data), "\n\n") {
		t.Fatalf("fixture must contain exactly one trailing LF")
	}
	canonicalFixture := append(append([]byte(nil), wire...), '\n')
	if string(canonicalFixture) != string(data) {
		t.Fatalf("fixture is not canonical closed JSON with one trailing LF: got=%s want=%s", canonicalFixture, data)
	}
	digest := sha256.Sum256(data)
	if got, want := hex.EncodeToString(digest[:]), "b8c04e213a2fe068e931f48480014a85e9bc42d7d482e0b2085d66e02b6eb3a8"; got != want {
		t.Fatalf("fixture SHA-256=%s, want %s", got, want)
	}
	if strings.Contains(string(data), "Bearer") || strings.Contains(string(data), "service-token") || strings.Contains(string(data), "Authorization") {
		t.Fatal("secret-shaped value found in canonical fixture")
	}
	delivery, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{
		TargetInstance: request.TargetInstance,
		Request:        request,
		AGSPrID:        request.ExternalRepo + "#" + strconv.Itoa(request.ExternalNumber),
		ObservedAt:     time.Unix(10, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Request.IdempotencyKey != request.IdempotencyKey {
		t.Fatalf("fixture idempotency key is not the canonical key: got=%s want=%s", delivery.Request.IdempotencyKey, request.IdempotencyKey)
	}
}

func TestDecodeExternalPRTerminalDeliveryRejectsUnknownFields(t *testing.T) {
	delivery, err := NewExternalPRTerminalDelivery(ExternalPRTerminalDeliveryInput{
		TargetInstance: "mini-prod",
		Request:        terminalRequest("closed", ""),
		AGSPrID:        "owner/repo#7",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalExternalPRTerminalDelivery(delivery)
	if err != nil {
		t.Fatal(err)
	}
	payload = strings.TrimSuffix(payload, "}") + `,"unknown":true}`
	if _, err := DecodeExternalPRTerminalDelivery([]byte(payload)); err == nil {
		t.Fatal("expected closed wrapper decoder to reject unknown field")
	}
}
