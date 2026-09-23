package main

import "testing"

func TestFixtureShapeIgnoresNarrativeButRetainsAssertions(t *testing.T) {
	before := []byte("package sample\nimport \"testing\"\nfunc TestDenied(t *testing.T) { if got != \"old-fixture\" { t.Fatal(\"denied\") } }\n")
	a, err := sourceShape("fixture_test.go", before)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sourceShape("fixture_test.go", []byte("package sample\n// synthetic fixture\nimport \"testing\"\nfunc TestDenied(t *testing.T) { if got != \"example-fixture\" { t.Fatal(\"denied\") } }\n"))
	if err != nil || !sameStrings(a.Tokens, b.Tokens) || !sameStrings(a.Imports, b.Imports) || !sameStrings(a.Tests, b.Tests) {
		t.Fatal("narrative-only replacement changed the fixture structure", err)
	}
	for name, source := range map[string]string{
		"inverted permission assertion": "package sample\nimport \"testing\"\nfunc TestDenied(t *testing.T) { if got == \"example-fixture\" { t.Fatal(\"denied\") } }\n",
		"removed test":                  "package sample\nimport \"testing\"\n",
		"changed failure behavior":      "package sample\nimport \"testing\"\nfunc TestDenied(t *testing.T) { if got != \"example-fixture\" { t.Log(\"denied\") } }\n",
		"changed import":                "package sample\nimport \"other/import\"\nfunc TestDenied(t *testing.T) { if got != \"example-fixture\" { t.Fatal(\"denied\") } }\n",
	} {
		t.Run(name, func(t *testing.T) {
			changed, err := sourceShape("fixture_test.go", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			if sameStrings(a.Tokens, changed.Tokens) && sameStrings(a.Imports, changed.Imports) && sameStrings(a.Tests, changed.Tests) {
				t.Fatal("unsafe structural change was accepted")
			}
		})
	}
}
