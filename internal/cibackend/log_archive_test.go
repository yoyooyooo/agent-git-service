package cibackend

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestLogArchiveChecksExpandedBudgetAndTraversal(t *testing.T) {
	archive := func(name, text string) []byte {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		f, e := w.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = f.Write([]byte(text))
		if e = w.Close(); e != nil {
			t.Fatal(e)
		}
		return b.Bytes()
	}
	if e := ValidateLogArchive(archive("0_test.txt", "fixture\n")); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"../escape.txt", "/absolute.txt", "nested/../escape.txt", "nested\\escape.txt"} {
		if e := ValidateLogArchive(archive(name, "x")); !errors.Is(e, ErrInvalid) {
			t.Fatal(name, e)
		}
	}
	// Repeated bytes compress to a small archive but must still hit the decoded
	// aggregate limit. The validator does not extract anything into the filesystem.
	if e := ValidateLogArchive(archive("0_bomb.txt", strings.Repeat("x", MaxBody+1))); !errors.Is(e, ErrUnavailable) {
		t.Fatal("expanded log budget not enforced", e)
	}
}
