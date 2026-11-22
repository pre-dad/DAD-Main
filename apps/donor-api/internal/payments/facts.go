package payments

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

var ErrFacts = errors.New("invalid payment facts")

// maxUnix is 9999-12-31T23:59:59Z, the last second PostgreSQL timestamps and
// RFC 3339 can both represent.
const maxUnix = 253402300799

// Object is a processor object kept as raw JSON, so only the fields a caller
// verifies are ever decoded.
type Object map[string]json.RawMessage

// DecodeObject parses one JSON object; anything else, including null, is ErrFacts.
func DecodeObject(data []byte) (Object, error) {
	var value Object
	if err := json.Unmarshal(data, &value); err != nil || value == nil {
		return nil, ErrFacts
	}
	return value, nil
}

// Absent reports whether key is missing or null.
func (o Object) Absent(key string) bool {
	raw := bytes.TrimSpace(o[key])
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

// Text returns key as a string, or "" when it is missing or not a string.
func (o Object) Text(key string) string {
	var value string
	if json.Unmarshal(o[key], &value) != nil {
		return ""
	}
	return value
}

// Int returns key as an integer; a missing, null or non-integer value is ErrFacts.
func (o Object) Int(key string) (int64, error) {
	var value int64
	if o.Absent(key) || json.Unmarshal(o[key], &value) != nil {
		return 0, ErrFacts
	}
	return value, nil
}

// IntDefault is Int, except that a missing key is zero. A key present as null is
// still ErrFacts, so a malformed fact is never read as zero.
func (o Object) IntDefault(key string) (int64, error) {
	if _, ok := o[key]; !ok {
		return 0, nil
	}
	return o.Int(key)
}

// Unix reads key as a Unix timestamp in seconds.
func (o Object) Unix(key string) (time.Time, error) {
	seconds, err := o.Int(key)
	if err != nil || seconds < 0 || seconds > maxUnix {
		return time.Time{}, ErrFacts
	}
	return time.Unix(seconds, 0).UTC(), nil
}

// BoolIs reports whether key is present as the JSON boolean want.
func (o Object) BoolIs(key string, want bool) bool {
	expected := "false"
	if want {
		expected = "true"
	}
	return string(bytes.TrimSpace(o[key])) == expected
}

// Object returns key as a nested object; a missing or null key is ErrFacts.
func (o Object) Object(key string) (Object, error) { return DecodeObject(o[key]) }

// Array returns key as a list of objects. A null value is an empty list and a
// missing key is ErrFacts, so callers must check the length they expect.
func (o Object) Array(key string) ([]Object, error) {
	var value []Object
	if err := json.Unmarshal(o[key], &value); err != nil {
		return nil, ErrFacts
	}
	return value, nil
}
