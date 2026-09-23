package githttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/gitbackend"
)

// Keep the primary's existing spool regression coverage against the extracted
// backend; extraction must not change the environment configuration contract.
func TestSpoolChunkedBody(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  string
		cap      int64
		exceeded bool
	}{
		{"under_cap", "hello world", 1024, false},
		{"exactly_at_cap", strings.Repeat("x", 64), 64, false},
		{"exceeds_cap", strings.Repeat("x", 2048), 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("GITHTTP_SPOOL_DIR", dir)
			rr := httptest.NewRecorder()
			tmp, n, exceeded, err := gitbackend.SpoolChunkedBody(context.Background(), rr, io.NopCloser(strings.NewReader(tc.payload)), tc.cap)
			if err != nil || exceeded != tc.exceeded {
				t.Fatalf("exceeded=%v err=%v", exceeded, err)
			}
			if exceeded {
				if tmp != nil || rr.Code != http.StatusRequestEntityTooLarge {
					t.Fatalf("tmp=%v status=%d", tmp, rr.Code)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("overflow leaked spool files: %v, %v", entries, err)
				}
				return
			}
			defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
			data, err := io.ReadAll(tmp)
			if err != nil || !bytes.Equal(data, []byte(tc.payload)) || n != int64(len(tc.payload)) {
				t.Fatalf("size=%d data=%q err=%v", n, data, err)
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d", rr.Code)
			}
		})
	}
	t.Run("read_error_propagates", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("GITHTTP_SPOOL_DIR", dir)
		_, _, _, err := gitbackend.SpoolChunkedBody(context.Background(), httptest.NewRecorder(), io.NopCloser(&errReader{}), 1024)
		if err == nil {
			t.Fatal("expected reader error")
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("read error leaked spool files: %v, %v", entries, err)
		}
	})
}

func TestSpoolChunkedBodyHonorsSpoolDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GITHTTP_SPOOL_DIR", dir)
	tmp, _, _, err := gitbackend.SpoolChunkedBody(context.Background(), httptest.NewRecorder(), io.NopCloser(strings.NewReader("payload")), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	if filepath.Dir(tmp.Name()) != dir {
		t.Fatalf("spool path=%q want directory %q", tmp.Name(), dir)
	}
}

func TestMaxPushBytes(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", gitbackend.DefaultMaxPushBytes},
		{"1024", 1024},
		{"not-a-number", gitbackend.DefaultMaxPushBytes},
		{"0", gitbackend.DefaultMaxPushBytes},
	} {
		t.Setenv("GITHTTP_MAX_PUSH_BYTES", tc.value)
		if got := gitbackend.MaxPushBytes(); got != tc.want {
			t.Errorf("override=%q got=%d want=%d", tc.value, got, tc.want)
		}
	}
}

type errReader struct{}

func (*errReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
