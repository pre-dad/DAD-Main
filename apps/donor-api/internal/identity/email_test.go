package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestLegacyEmailCompatibility(t *testing.T) {
	var fixture struct {
		Digest string `json:"scalar_digest"`
		Cases  []struct {
			Input    string  `json:"input"`
			Expected *string `json:"expected"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("../../testdata/identity.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		got, err := NormalizeEmail(tc.Input)
		if tc.Expected == nil {
			if err == nil {
				t.Fatalf("legacy contract rejected %q, got %q", tc.Input, got)
			}
		} else if err != nil || got != *tc.Expected {
			t.Fatalf("%q: got %q (%v), want %q", tc.Input, got, err, *tc.Expected)
		}
	}
	// This digest was produced by the legacy identity contract's normalize_email,
	// independently of the exported tables, for every Unicode scalar as a single-label domain.
	digest := sha256.New()
	for r := rune(0); r < 0x110000; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		value, err := NormalizeEmail("x@" + string(r) + ".org")
		if err != nil {
			value = "!"
		}
		digest.Write([]byte(value + "\n"))
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != fixture.Digest {
		t.Fatalf("Unicode scalar compatibility drift: got %s, want %s", got, fixture.Digest)
	}
}

func TestNormalizeEmailBoundsLength(t *testing.T) {
	long := "x@" + strings.Repeat("\u0315", maxEmailRunes) + ".org"
	if _, err := NormalizeEmail(long); err == nil {
		t.Fatal("an address over the length bound was normalized")
	}
	if _, err := NormalizeEmail(strings.Repeat("a", 64) + "@example.org"); err != nil {
		t.Fatalf("a maximum-length local part was refused: %v", err)
	}
}
