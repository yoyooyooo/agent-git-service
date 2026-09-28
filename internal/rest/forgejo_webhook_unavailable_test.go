package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/service"
)

func TestForgejoWebhookUnavailableIsNotAnInvalidPayload(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"deadline", context.DeadlineExceeded, http.StatusServiceUnavailable},
		{"cancel", context.Canceled, http.StatusServiceUnavailable},
		{"provider read", fmt.Errorf("private-provider-diagnostic: %w", service.ErrForgejoActionObservationUnavailable), http.StatusServiceUnavailable},
		{"signature/payload", errors.New("invalid private diagnostic"), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			respondForgejoWebhookFailure(w, test.err)
			if w.Code != test.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private") {
				t.Fatal("private diagnostic leaked")
			}
			if w.Code == 503 && !strings.Contains(w.Body.String(), "existing action") {
				t.Fatal("missing safe recovery direction")
			}
		})
	}
}
