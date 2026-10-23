package gitbackend

import (
	"io"
	"log"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Keep the adapted host's HTTP result byte-for-byte aligned with net/http/cgi,
// including malformed headers and failing children. Exit observation must not
// change status, headers, body, redirects, or create an extra error response.
func TestObservedCGIMatchesStandardLibrary(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		code         int
	}{
		{"normal", "printf 'Content-Type: application/octet-stream\r\n\r\n0008NAK\nPACKpartial'", 0},
		{"exit 7", "printf 'Content-Type: application/octet-stream\r\n\r\n0008NAK\nPACKpartial'; exit 7", 7},
		{"signal", "printf 'Content-Type: application/octet-stream\r\n\r\n0008NAK\nPACKpartial'; kill -KILL $$", -1},
		{"explicit status", "printf 'Status: 403 Forbidden\r\nContent-Type: text/plain\r\nX-Test: first\r\nX-Test: second\r\n\r\ndenied'", 0},
		{"redirect", "printf 'Location: /next\r\n\r\nredirect'", 0},
		{"bad status", "printf 'Status: invalid\r\n\r\nignored'; exit 7", 7},
		{"missing header", "printf 'bad header\r\n\r\nignored'", 0},
		{"no header", "printf 'not a CGI response'", 0},
		{"invalid header ignored", "printf 'bad header\r\nInvalid Name: x\r\nContent-Type: text/plain\r\n\r\nok'", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "backend")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			config := cgi.Handler{Path: path, Logger: log.New(io.Discard, "", 0)}
			standard := httptest.NewRecorder()
			config.ServeHTTP(standard, httptest.NewRequest("GET", "http://cgi.example.test/repo.git/info/refs", nil))
			calls := 0
			observed := cgiHandler{Handler: config, onExit: func(state *os.ProcessState, err error) {
				calls++
				if state == nil || state.ExitCode() != tc.code || (err != nil) != (tc.code != 0) {
					t.Errorf("exit: state=%v err=%v", state, err)
				}
			}}
			actual := httptest.NewRecorder()
			observed.ServeHTTP(actual, httptest.NewRequest("GET", "http://cgi.example.test/repo.git/info/refs", nil))
			if calls != 1 || standard.Code != actual.Code || !reflect.DeepEqual(standard.Header(), actual.Header()) || standard.Body.String() != actual.Body.String() {
				t.Fatalf("CGI changed response: standard=%v actual=%v calls=%d", standard, actual, calls)
			}
		})
	}
}
