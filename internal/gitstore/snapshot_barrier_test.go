package gitstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSnapshotBarrierWaitsForNestedMutationLifetime(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, releaseOuter, err := s.BeginMutation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, releaseInner, err := s.BeginMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseInner()
	defer releaseOuter()
	releaseOuter()
	releaseOuter() // idempotent; nested work still holds barrier
	wait, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.WithSnapshotCapture(wait, func(context.Context) error { t.Error("capture passed active nested mutation"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout, got %v", err)
	}
	releaseInner()
	if err := s.WithSnapshotCapture(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Reusing a context AFTER all leases finish must acquire a new permit.
	_, releaseNew, err := s.BeginMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	releaseNew()
}

func TestSnapshotBarrierBlocksRealWritesAndNestedCaptureFails(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background(), "owner/repo", "main", true); err != nil {
		t.Fatal(err)
	}
	before, err := s.HeadSHA(context.Background(), "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithSnapshotCapture(context.Background(), func(captured context.Context) error {
		if _, release, err := s.BeginMutation(captured); err == nil {
			release()
			t.Error("capture callback could mutate")
		}
		if err := s.WithSnapshotCapture(captured, func(context.Context) error { return nil }); err == nil {
			t.Error("nested capture admitted")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if _, err := s.WriteFile(ctx, "owner/repo", "main", "blocked", "blocked", []byte("x")); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("write escaped barrier: %v", err)
		}
		if got, _ := s.HeadSHA(captured, "owner/repo", "main"); got != before {
			t.Error("refs changed inside capture")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(context.Background(), "owner/repo", "main", "after", "after", []byte("ok")); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotBarrierNestedMutationDoesNotQueueBehindWriter(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer, release, err := s.BeginMutation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	captureCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.WithSnapshotCapture(captureCtx, func(context.Context) error { return nil }) }()
	// Nested acquisition is lexical/refcounted, independent of writer queue.
	nestedCtx, nestedCancel := context.WithTimeout(outer, 100*time.Millisecond)
	defer nestedCancel()
	_, nestedRelease, err := s.BeginMutation(nestedCtx)
	if err != nil {
		t.Fatal(err)
	}
	nestedRelease()
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
