package snapshotstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestHeadroomGateRefusesImportWithoutDamagingPublishedView(t *testing.T) {
	source, m := testSource(t, "sha1", true)
	s := openTestStore(t, 0)
	base := importSource(t, s, source, m)
	free, err := FilesystemAvailable(s.root)
	if err != nil || free == 0 {
		t.Fatal("statfs unavailable", err)
	}
	if err := s.ConfigureHeadroom(1); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureHeadroom(1 << 60); !errors.Is(err, ErrHeadroom) {
		t.Fatalf("impossible headroom accepted: %v", err)
	}
	called := false
	err = s.Install(context.Background(), m, func(context.Context, io.Writer) error { called = true; return nil })
	if !errors.Is(err, ErrHeadroom) || called {
		t.Fatal("import producer ran despite headroom refusal", err)
	}
	if got := gitOK(t, base.RepoPath(), "", "rev-parse", "HEAD"); got != m.Snapshot.HEAD.OID {
		t.Fatal("existing view lost")
	}
}
func TestHeadroomIsRecheckedWhileWritingNotOnlyAtStartup(t *testing.T) {
	var dst bytes.Buffer
	checks := 0
	w := &headroomWriter{writer: &dst, check: func() error {
		checks++
		if checks == 2 {
			return ErrHeadroom
		}
		return nil
	}}
	chunk := make([]byte, 4<<20)
	if _, err := w.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("next")); !errors.Is(err, ErrHeadroom) {
		t.Fatal("runtime low-space not rejected", err)
	}
	if dst.Len() != len(chunk) {
		t.Fatal("wrote below headroom threshold")
	}
}
