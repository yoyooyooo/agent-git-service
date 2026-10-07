package service_test

import (
	"testing"

	"github.com/ngaut/agent-git-service/internal/service"
)

func TestConfiguredBrowserAndAPIOriginsRemainIndependent(t *testing.T) {
	for _, origin := range []string{
		"http://git.example.test:6666",
		"https://git.example.test",
		"https://git.example.test:8443",
	} {
		t.Run(origin, func(t *testing.T) {
			svc := &service.Service{BaseURL: origin}
			if got := svc.HTMLBaseURL(); got != origin {
				t.Fatalf("browser origin = %q, want configured origin %q", got, origin)
			}
			if got := svc.APIBaseURL(); got != origin {
				t.Fatalf("default API origin = %q, want configured origin %q", got, origin)
			}
			svc.PublicAPIBaseURL = "https://api.example.test"
			if got := svc.HTMLBaseURL(); got != origin {
				t.Fatalf("API override changed browser origin to %q", got)
			}
			if got := svc.APIBaseURL(); got != "https://api.example.test" {
				t.Fatalf("API override was not preserved: %q", got)
			}
		})
	}
}
