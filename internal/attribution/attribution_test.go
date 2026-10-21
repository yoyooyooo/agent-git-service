package attribution

import (
	"context"
	"strings"
	"testing"
)

func TestMappingValidationAndReverseLookup(t *testing.T) {
	for _, cfg := range []Config{
		{Principals: map[string]Principal{"a": {Forgejo: "same"}, "b": {Forgejo: "SAME"}}},
		{Principals: map[string]Principal{"bad\nheader": {Forgejo: "remote"}}},
		{Principals: map[string]Principal{"a": {Forgejo: "remote/user"}}},
	} {
		if cfg.Validate() == nil {
			t.Fatal("invalid mapping accepted")
		}
	}
	c := Config{Principals: map[string]Principal{"alice": {Forgejo: "alice.remote", GitHub: "alice-gh"}}}
	if c.Validate() != nil || c.AGSLogin("ALICE.REMOTE") != "alice" || c.AGSLogin("unknown") != "forgejo" {
		t.Fatal("reverse mapping failed")
	}
}

func TestMarkerReplacementAndMergeTrailer(t *testing.T) {
	a := Actor{UserID: 7, Login: "alice"}
	body := Mark("body", a, "github", "alice-gh", "text-only")
	body = Mark(body, a, "github", "alice-gh", "text-only")
	if strings.Count(body, "AGS-Actor:") != 1 || !strings.HasSuffix(body, "\nbody") || !strings.Contains(body, "github=alice-gh") {
		t.Fatal(body)
	}
	original := "user\nAGS-Actor: quoted user text"
	if !strings.HasSuffix(Mark(original, a, "forgejo", "", "unmapped"), original) {
		t.Fatal("user body changed")
	}
	trailer := MergeTrailer("Merge PR\n\nAGS-Actor: old", "alice")
	if trailer != "Merge PR\n\nAGS-Actor: alice" {
		t.Fatal(trailer)
	}
	ctx := WithActor(context.Background(), a)
	if FromContext(ctx).RequestID == "" {
		t.Fatal("missing request id")
	}
}
