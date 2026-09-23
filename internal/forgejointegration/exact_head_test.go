package forgejointegration

import (
	"strings"
	"testing"
)

func TestProjectedHeadVerificationRejectsAbbreviatedObjectIDs(t *testing.T) {
	full := strings.Repeat("a", 40)
	if err := verifyProjectedHeadSHA(full, full, "feature/example"); err != nil {
		t.Fatal("exact full object identity was rejected", err)
	}
	for _, pair := range [][2]string{
		{full, full[:7]},
		{full[:7], full},
		{strings.Repeat("b", 64), strings.Repeat("b", 40)},
	} {
		if err := verifyProjectedHeadSHA(pair[0], pair[1], "feature/example"); err == nil {
			t.Fatal("a matching prefix was accepted as exact projected head identity")
		}
	}
}
