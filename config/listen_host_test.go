package config

import (
	"strings"
	"testing"
)

func TestListenHostNormalization(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", ""},
		{" \t\n", ""},
		{" 127.0.0.1 \t", "127.0.0.1"},
		{"0.0.0.0", "0.0.0.0"},
		{" ::1 ", "::1"},
		{"::", "::"},
		{"2001:db8::1", "2001:db8::1"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := Normalize(Config{
				DBdsn:      "unused-test-dsn",
				ListenHost: tc.raw,
				Port:       "9090",
				BaseURL:    "https://git.example.test",
				APIBaseURL: "https://api.example.test",
			})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ListenHost != tc.want {
				t.Fatalf("ListenHost=%q, want %q", cfg.ListenHost, tc.want)
			}
			if cfg.Port != "9090" || cfg.BaseURL != "https://git.example.test" || cfg.APIBaseURL != "https://api.example.test" {
				t.Fatal("listener binding changed the port or advertised origins")
			}
		})
	}
}

func TestListenHostRejectsNonIPValues(t *testing.T) {
	for _, raw := range []string{
		"localhost", "example.test", "https://127.0.0.1", "127.0.0.1:8080",
		"[::1]", "[::1]:8080", "fe80::1%lo0", "127.0.0.1/8", "999.0.0.1", "127.0.0.1,::1",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := Normalize(Config{DBdsn: "unused-test-dsn", ListenHost: raw})
			if err == nil || !strings.Contains(err.Error(), "AGS_LISTEN_HOST") {
				t.Fatalf("invalid ListenHost accepted or misdiagnosed: %v", err)
			}
		})
	}
}
