package edgeprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareReadBindingAndClosedControlJSON(t *testing.T) {
	snapshot := sampleSnapshot()
	base := PrepareRead{Version: PrepareVersion, RequestID: "request-1", Repository: "owner/repo", Identity: snapshot.Identity, Phase: "discover"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	fetch := base
	fetch.Phase = "fetch"
	fetch.Snapshot = &snapshot
	if err := fetch.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*PrepareRead){
		func(p *PrepareRead) { p.Repository = "../repo" },
		func(p *PrepareRead) { p.RequestID = "" },
		func(p *PrepareRead) { p.Phase = "fetch" },
		func(p *PrepareRead) { p.Snapshot = &snapshot },
		func(p *PrepareRead) { p.Phase = "push" },
	} {
		p := base
		mutate(&p)
		if p.Validate() == nil {
			t.Fatal("invalid prepare accepted")
		}
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PrepareRead
	if err := DecodeControl(strings.NewReader(string(encoded)), &decoded); err != nil || decoded.Validate() != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	for _, body := range []string{
		string(encoded) + " {}",
		strings.Replace(string(encoded), "\"request_id\":", "\"request_id\":\"duplicate\",\"request_id\":", 1),
		strings.Replace(string(encoded), "\"request_id\":", "\"Request_ID\":\"duplicate\",\"request_id\":", 1),
		strings.Replace(string(encoded), "\"repository_id\":", "\"repository_id\":999,\"repository_id\":", 1),
		strings.Replace(string(encoded), "\"request_id\":", "\"user_token\":\"forbidden\",\"request_id\":", 1),
		strings.Repeat(" ", MaxControlBytes+1),
		strings.Repeat("[", 20) + "0" + strings.Repeat("]", 20),
	} {
		var invalid PrepareRead
		if DecodeControl(strings.NewReader(body), &invalid) == nil {
			t.Fatal("ambiguous/unknown/oversized envelope accepted")
		}
	}
}
