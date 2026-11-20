package geo

import (
	"errors"
	"testing"
	"time"
)

func TestNormalizeZip(t *testing.T) {
	for _, in := range []string{"02139", "94103", "10001-1234"} {
		if got, err := NormalizeZip(in); err != nil || got != in {
			t.Errorf("NormalizeZip(%q) = %q, %v; want %q, nil", in, got, err, in)
		}
	}
	for _, in := range []string{"", "1234", "123456", "abcde", "12345-12", "9410a"} {
		if _, err := NormalizeZip(in); !errors.Is(err, ErrZip) {
			t.Errorf("NormalizeZip(%q) error = %v; want ErrZip", in, err)
		}
	}
}

func TestTimezoneByPrefix(t *testing.T) {
	cases := map[string]string{
		"10001":      "America/New_York",    // NYC
		"02139":      "America/New_York",    // Cambridge MA
		"60601":      "America/Chicago",     // Chicago
		"80202":      "America/Denver",      // Denver
		"85001":      "America/Phoenix",     // Phoenix (no DST)
		"94103":      "America/Los_Angeles", // San Francisco
		"98101":      "America/Los_Angeles", // Seattle
		"99501":      "America/Anchorage",   // Anchorage
		"96801":      "Pacific/Honolulu",    // Honolulu
		"00601":      "America/Puerto_Rico", // PR
		"10001-1234": "America/New_York",    // ZIP+4 resolves on the five-digit core
	}
	for zip, want := range cases {
		got, err := Timezone(zip)
		if err != nil {
			t.Errorf("Timezone(%q) error = %v", zip, err)
			continue
		}
		if got != want {
			t.Errorf("Timezone(%q) = %q; want %q", zip, got, want)
		}
	}
}

func TestTimezoneOverrides(t *testing.T) {
	// Each of these is in a prefix region whose default zone differs from the
	// override; the full-ZIP override must win.
	cases := map[string]string{
		"32401": "America/Chicago",     // FL panhandle (region default eastern)
		"37902": "America/New_York",    // Knoxville TN (region default central)
		"46320": "America/Chicago",     // Hammond IN (region default eastern)
		"79901": "America/Denver",      // El Paso TX (region default central)
		"83814": "America/Los_Angeles", // Coeur d'Alene ID (region default mountain)
	}
	for zip, want := range cases {
		got, err := Timezone(zip)
		if err != nil {
			t.Fatalf("Timezone(%q) error = %v", zip, err)
		}
		if got != want {
			t.Errorf("Timezone(%q) = %q; want %q (override)", zip, got, want)
		}
	}
}

func TestTimezoneUnresolvable(t *testing.T) {
	// Well-formed but unassigned/territorial prefixes the table does not cover.
	for _, zip := range []string{"00000", "00100"} {
		if _, err := Timezone(zip); !errors.Is(err, ErrZip) {
			t.Errorf("Timezone(%q) error = %v; want ErrZip", zip, err)
		}
	}
	if _, err := Timezone("bad"); !errors.Is(err, ErrZip) {
		t.Errorf("Timezone(bad) error = %v; want ErrZip", err)
	}
}

func TestEveryZoneLoads(t *testing.T) {
	// The embedded table's init already panics on a bad zone; assert it here too
	// so a regression surfaces as a test failure, not a startup crash.
	seen := map[string]bool{}
	for _, name := range zones.Prefix {
		seen[name] = true
	}
	for _, name := range zones.Override {
		seen[name] = true
	}
	if len(seen) == 0 {
		t.Fatal("no zones loaded")
	}
	for name := range seen {
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("zone %q does not load: %v", name, err)
		}
	}
}

func TestEveryResolvableZoneIsAValidOverride(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range zones.Prefix {
		seen[name] = true
	}
	for _, name := range zones.Override {
		seen[name] = true
	}
	for name := range seen {
		if !ValidTimezoneOverride(name) {
			t.Errorf("table zone %q is not an accepted profile override", name)
		}
	}
	if ValidTimezoneOverride("UTC") {
		t.Error("UTC accepted as a profile time zone")
	}
}
