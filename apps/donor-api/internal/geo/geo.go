// Package geo estimates a US ZIP's IANA time zone from three-digit regions
// and full-ZIP exceptions. Border ZIPs without exceptions use the region's
// majority zone; a donor can correct that suggestion in their profile.
package geo

import (
	_ "embed"
	"encoding/json"
	"errors"
	"regexp"
	"time"
	_ "time/tzdata"
)

// ErrZip is returned for input that is not a US ZIP the table can resolve.
var ErrZip = errors.New("enter a valid US ZIP code")

// zipPattern accepts ZIP5 or ZIP+4; only the five-digit core is used for the
// lookup. It mirrors the ck_donor_zip database constraint.
var zipPattern = regexp.MustCompile(`^[0-9]{5}(-[0-9]{4})?$`)

//go:embed zones.json
var encodedZones []byte

type zoneTable struct {
	// Prefix maps a three-digit ZIP prefix ("0" through "999", left-padded) to
	// an IANA zone name.
	Prefix map[string]string `json:"prefix"`
	// Override maps a full five-digit ZIP to a zone, for the ZIPs whose prefix
	// region straddles a time-zone boundary.
	Override map[string]string `json:"override"`
}

var zones = func() zoneTable {
	var t zoneTable
	if err := json.Unmarshal(encodedZones, &t); err != nil {
		panic("invalid embedded geo zones")
	}
	// Every zone the table names must load, so a bad name fails at startup, not
	// when the first donor in that region saves a profile.
	for _, name := range t.Prefix {
		if _, err := time.LoadLocation(name); err != nil {
			panic("geo: unknown IANA zone " + name)
		}
	}
	for _, name := range t.Override {
		if _, err := time.LoadLocation(name); err != nil {
			panic("geo: unknown IANA zone " + name)
		}
	}
	return t
}()

// NormalizeZip returns the canonical ZIP form (digits, optional +4 hyphen) or
// ErrZip. It does not confirm the ZIP resolves to a zone; use Timezone for that.
func NormalizeZip(zip string) (string, error) {
	if !zipPattern.MatchString(zip) {
		return "", ErrZip
	}
	return zip, nil
}

// Timezone returns the IANA zone name for a ZIP: the full-ZIP override when one
// exists, otherwise the ZIP's three-digit-prefix region. A ZIP whose region is
// not in the table (an unassigned or territorial range) returns ErrZip rather
// than a wrong guess.
func Timezone(zip string) (string, error) {
	if !zipPattern.MatchString(zip) {
		return "", ErrZip
	}
	five := zip[:5]
	if name, ok := zones.Override[five]; ok {
		return name, nil
	}
	if name, ok := zones.Prefix[five[:3]]; ok {
		return name, nil
	}
	return "", ErrZip
}

// ValidTimezoneOverride restricts profile corrections to the supported US civil time zones.
func ValidTimezoneOverride(zone string) bool {
	switch zone {
	case "America/New_York",
		"America/Chicago",
		"America/Denver",
		"America/Phoenix",
		"America/Los_Angeles",
		"America/Anchorage",
		"America/Adak",
		"Pacific/Honolulu",
		"America/Puerto_Rico",
		"Pacific/Guam",
		"Pacific/Pago_Pago":
		return true
	default:
		return false
	}
}
