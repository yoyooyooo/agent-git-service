package edge

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func readPkt(line string) string { return fmt.Sprintf("%04x%s\n", len(line)+5, line) }

func TestReadRequestClassifiesNativePhasesAndPreservesBytes(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, version, body, phase string
		wants                      int
	}{
		{"v0 clone", "", readPkt("want "+oid+" multi_ack_detailed side-band-64k ofs-delta") + "0000" + readPkt("done"), "fetch", 1},
		{"v0 shallow", "version=1", readPkt("want "+oid+" multi_ack") + readPkt("deepen 1") + "0000", "fetch", 1},
		{"v0 negotiation", "", readPkt("want "+oid) + "0000" + readPkt("have "+oid) + "0000", "fetch", 1},
		{"v2 discovery", "version=2", readPkt("command=ls-refs") + readPkt("agent=git/test") + "0001" + readPkt("peel") + readPkt("symrefs") + readPkt("ref-prefix refs/heads/") + "0000", "discover", 0},
		{"v2 fetch", "version=2", readPkt("command=fetch") + readPkt("object-format=sha1") + "0001" + readPkt("thin-pack") + readPkt("want "+oid) + readPkt("done") + "0000", "fetch", 1},
		{"v2 shallow", "version=2", readPkt("command=fetch") + "0001" + readPkt("shallow "+oid) + readPkt("deepen 2147483647") + readPkt("want "+oid) + readPkt("done") + "0000", "fetch", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://ags.test/a/b.git/git-upload-pack", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			r.Header.Set("Git-Protocol", tc.version)
			got, err := parseReadRequest(r)
			if err != nil || got.phase != tc.phase || len(got.wants) != tc.wants || string(got.body) != tc.body {
				t.Fatalf("phase=%s wants=%v err=%v", got.phase, got.wants, err)
			}
		})
	}
}

func TestReadRequestRejectsAmbiguityAndUnadvertisedCommands(t *testing.T) {
	oid := strings.Repeat("a", 40)
	v2 := func(args string) string { return readPkt("command=fetch") + "0001" + args + "0000" }
	for _, body := range []string{
		"", "000", "0003", "0004", "ffffx", "zzzz", "0002", "0000trailing",
		readPkt("command=fetch") + "0001" + readPkt("want "+oid),
		readPkt("command=ls-refs") + "0000",
		readPkt("command=object-info") + "0001" + readPkt("size") + "0000",
		readPkt("command=ls-refs") + readPkt("command=fetch") + "0001" + "0000",
		v2(readPkt("want "+oid) + readPkt("filter blob:none")),
		v2(readPkt("want "+oid) + readPkt("want-ref refs/heads/main")),
		v2(readPkt("want "+oid) + readPkt("packfile-uris https")),
		v2(readPkt("want " + strings.Repeat("0", 40))),
		v2(readPkt("want " + oid + " extra")),
		v2(readPkt("want "+oid) + "0001"),
		v2(readPkt("want "+oid)) + v2(readPkt("want "+oid)),
		v2(readPkt("want "+oid) + readPkt("have invalid")),
		v2(readPkt("want "+oid) + readPkt("deepen -1")),
	} {
		r := httptest.NewRequest("POST", "http://ags.test/a/b.git/git-upload-pack", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		r.Header.Set("Git-Protocol", "version=2")
		if _, err := parseReadRequest(r); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	for _, header := range []string{"version=3", "version=2:version=0", "version=2,version=0"} {
		r := httptest.NewRequest("GET", "http://ags.test/a/b.git/info/refs?service=git-upload-pack", nil)
		r.Header.Set("Git-Protocol", header)
		if _, err := parseReadRequest(r); err == nil {
			t.Errorf("accepted header %q", header)
		}
	}
}

func TestReadRequestGzipIsBoundedAndSingleStream(t *testing.T) {
	body := readPkt("command=ls-refs") + "0001" + readPkt("symrefs") + "0000"
	zip := func(data string) []byte {
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		if _, err := io.WriteString(z, data); err != nil {
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	for _, tc := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{"valid", zip(body), true},
		{"trailing", append(zip(body), 'x'), false},
		{"second stream", append(zip(body), zip(body)...), false},
		{"decoded overflow", zip(strings.Repeat("x", maxReadRequestBytes+1)), false},
		{"encoded overflow", bytes.Repeat([]byte("x"), maxReadRequestBytes+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://ags.test/a/b.git/git-upload-pack", bytes.NewReader(tc.data))
			r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			r.Header.Set("Git-Protocol", "version=2")
			r.Header.Set("Content-Encoding", "gzip")
			got, err := parseReadRequest(r)
			if (err == nil) != tc.ok || tc.ok && string(got.body) != body {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func FuzzGitReadPackets(f *testing.F) {
	f.Add([]byte("0000"))
	f.Add([]byte(readPkt("command=ls-refs") + "0001" + readPkt("symrefs") + "0000"))
	f.Add([]byte(readPkt("want "+strings.Repeat("a", 40)) + "0000" + readPkt("done")))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxReadRequestBytes {
			return
		}
		packets, err := readPackets(data)
		if err != nil {
			return
		}
		var v0, v2 gitReadRequest
		_ = v0.parseV0(packets)
		_ = v2.parseV2(packets)
	})
}
