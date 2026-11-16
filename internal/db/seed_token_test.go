package db

import "testing"

func TestSeedTokenValue(t *testing.T) {
	for _, c := range []struct{ login, token, want string }{
		{"", "", "mytoken"},
		{"jackie", "secret", "secret"},
		{"jackie", "", ""},
		{"", "secret", ""},
	} {
		if got := SeedTokenValue(c.login, c.token); got != c.want {
			t.Fatalf("SeedTokenValue(%q,%q)=%q want %q", c.login, c.token, got, c.want)
		}
	}
}
