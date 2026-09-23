package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

func TestReplicationDrainDoesNotReleaseOwnershipBeforeHandlersExit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "retained")
	store, err := snapshotstore.Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	maintenanceDone := make(chan struct{})
	close(maintenanceDone)
	endpoint := &ReplicationEndpoint{retained: store, cancel: func() {}, done: maintenanceDone, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release })}
	ctx, cancel := context.WithCancel(context.Background())
	managed := &managedReplication{endpoint: endpoint, cancel: cancel, drained: make(chan struct{})}
	go func() {
		defer close(finished)
		managed.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://peer.test/", nil).WithContext(ctx))
	}()
	<-entered
	deadline, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = managed.close(deadline)
	stop()
	if err == nil {
		close(release)
		<-finished
		t.Fatal("reported drain success with active handler")
	}
	if another, err := snapshotstore.Open(root, 0); err == nil {
		another.Close()
		close(release)
		<-finished
		t.Fatal("active handler lost process ownership")
	}
	denied := httptest.NewRecorder()
	managed.ServeHTTP(denied, httptest.NewRequest("POST", "https://peer.test/", nil))
	if denied.Code != http.StatusServiceUnavailable {
		t.Fatal("new request admitted while draining")
	}
	close(release)
	<-finished
	if err := managed.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := snapshotstore.Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}
