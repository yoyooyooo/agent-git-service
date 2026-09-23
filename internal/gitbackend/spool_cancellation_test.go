package gitbackend

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type cancelSpoolBody struct {
	started chan struct{}
	closed  chan struct{}
	read    sync.Once
	close   sync.Once
}

func (b *cancelSpoolBody) Read([]byte) (int, error) {
	b.read.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *cancelSpoolBody) Close() error {
	b.close.Do(func() { close(b.closed) })
	return nil
}

func TestSpoolChunkedBodyCancelsBlockedReadAndRemovesTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GITHTTP_SPOOL_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &cancelSpoolBody{started: make(chan struct{}), closed: make(chan struct{})}
	defer body.Close()
	done := make(chan error, 1)
	go func() {
		file, _, _, err := SpoolChunkedBody(ctx, httptest.NewRecorder(), body, 1024)
		if file != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
		done <- err
	}()
	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatal("spool never started reading")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("cancel did not interrupt blocked request-body read")
		_ = body.Close()
		err = <-done // Join even when the implementation under test is broken.
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation cause was not preserved: %v", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("spool files survived cancellation: count=%d error=%v", len(entries), readErr)
	}
}

func TestSpoolChunkedBodyRejectsAlreadyCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	file, _, _, err := SpoolChunkedBody(ctx, httptest.NewRecorder(), io.NopCloser(strings.NewReader("not-consumed")), 1024)
	if file != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		t.Error("already-canceled request produced a spool file")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
