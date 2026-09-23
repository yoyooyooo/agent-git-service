package operationcatalog

import (
	"testing"
)

func TestStandardAndPrivilegedPartition(t *testing.T) {
	t.Parallel()

	for _, op := range []string{
		"repo.read", "git.read", "git.push", "pr.create", "pr.comment", "pr.edit",
		"pr.close", "pr.reopen", "pr.rebase", "review.read", "review.write", "ci.read",
	} {
		if !IsStandard(op) {
			t.Fatalf("%s should be standard", op)
		}
		if IsPrivileged(op) {
			t.Fatalf("%s should not be privileged", op)
		}
	}
	for _, op := range []string{
		"pr.merge", "review.dismiss", "git.force_push", "protected_ref.write",
		"ref.delete", "repo.create", "repo.delete", "repo.admin",
		"branch_protection.write", "webhook.write",
	} {
		if !IsPrivileged(op) {
			t.Fatalf("%s should be privileged", op)
		}
		if IsStandard(op) {
			t.Fatalf("%s should not be standard", op)
		}
	}
	if IsKnown("totally.unknown") {
		t.Fatal("unknown operation must fail closed")
	}
	if CanonicalName("review.submit") != "review.write" {
		t.Fatalf("review.submit hard-cut failed: %q", CanonicalName("review.submit"))
	}
}

func TestNoImplicitDefaultRisk(t *testing.T) {
	t.Parallel()
	for _, op := range AllOperations() {
		if _, ok := RiskOf(op); !ok {
			t.Fatalf("catalog entry %q missing risk", op)
		}
	}
}

func TestPRMergeRoleAcceptsMaintainerAndAdminOnly(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"maintainer", "admin", " ADMIN "} {
		if !IsPRMergeRole(role) {
			t.Fatalf("role %q should enable the single pr.merge capability", role)
		}
	}
	for _, role := range []string{"", "owner", "operator", "root"} {
		if IsPRMergeRole(role) {
			t.Fatalf("role %q must not enable pr.merge", role)
		}
	}
}
