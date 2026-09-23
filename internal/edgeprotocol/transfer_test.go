package edgeprotocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestTransferExactBindingsAndClosedFraming(t *testing.T) {
	s := sampleSnapshot()
	m, err := NewManifest(s.Identity, s.ObjectFormat, s.HEAD, s.ExportPolicyRevision, []Ref{{Name: s.HEAD.SymbolicRef, OID: s.HEAD.OID}})
	if err != nil {
		t.Fatal(err)
	}
	request := TransferRequest{Version: TransferVersion, Target: m.Snapshot, Base: &m.Snapshot}
	data, err := EncodeTransferRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeTransferRequest(bytes.NewReader(data))
	if err != nil || decoded.Target != request.Target || decoded.Base == nil || *decoded.Base != *request.Base {
		t.Fatal(err)
	}
	for _, body := range [][]byte{append(data, data...), []byte(strings.Replace(string(data), `"version":`, `"extra":true,"version":`, 1)), []byte(strings.Replace(string(data), `"version":`, `"version":"bad","version":`, 1))} {
		if _, err := DecodeTransferRequest(bytes.NewReader(body)); err == nil {
			t.Fatal("accepted ambiguous request")
		}
	}
	for _, change := range []func(*RepositorySnapshot){
		func(b *RepositorySnapshot) { b.Identity.StoreID = "different" },
		func(b *RepositorySnapshot) { b.Identity.AuthorityID = "different" },
		func(b *RepositorySnapshot) { b.ExportPolicyRevision = "different" },
		func(b *RepositorySnapshot) { b.Identity.Kind = "wiki" },
	} {
		base := m.Snapshot
		change(&base)
		if _, err := EncodeTransferRequest(TransferRequest{Version: TransferVersion, Target: m.Snapshot, Base: &base}); err == nil {
			t.Fatal("cross-boundary base accepted")
		}
	}
	for _, base := range []*RepositorySnapshot{nil, &m.Snapshot} {
		header := TransferHeader{Version: TransferVersion, Manifest: m, Base: base}
		var wire bytes.Buffer
		if err := WriteTransferHeader(&wire, header); err != nil {
			t.Fatal(err)
		}
		wire.WriteString("PACK")
		got, err := ReadTransferHeader(&wire)
		if err != nil || got.Manifest.Snapshot != m.Snapshot || (got.Base == nil) != (base == nil) || wire.String() != "PACK" {
			t.Fatalf("framing mismatch: %v", err)
		}
	}
	bad, _ := json.Marshal(TransferHeader{Version: TransferVersion, Manifest: m})
	for _, wire := range [][]byte{{0, 0, 0, 0}, {255, 255, 255, 255}, append([]byte{0, 0, 1, 0}, []byte("truncated")...)} {
		if _, err := ReadTransferHeader(bytes.NewReader(wire)); err == nil {
			t.Fatal("accepted bad framing")
		}
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(bad)+2))
	if _, err := ReadTransferHeader(bytes.NewReader(append(length[:], append(bad, []byte("{}")...)...))); err == nil {
		t.Fatal("accepted trailing header JSON")
	}
}
