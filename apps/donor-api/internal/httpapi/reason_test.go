package httpapi

import (
	"strings"
	"testing"
)

func TestValidReasonMeasuresTheTrimmedText(t *testing.T) {
	for reason, want := range map[string]bool{
		"":                             false,
		"    ok    ":                   false,
		"  long enough  ":              true,
		strings.Repeat("é", 500):       true,
		strings.Repeat("é", 501):       false,
		" " + strings.Repeat("é", 500): true,
	} {
		if got := validReason(reason); got != want {
			t.Errorf("validReason(%q) = %v; want %v", reason, got, want)
		}
	}
}
