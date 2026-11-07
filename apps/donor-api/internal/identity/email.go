// Package identity preserves the existing donor email identity contract. The
// IDNA2003 codec intentionally differs from modern UTS46/IDNA2008: changing that
// policy during a language rewrite could split one donor into two identities.
package identity

import (
	_ "embed"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Frozen from the former backend's stringprep and Unicode 3.2 normalization data.
// Changes require identity compatibility review, not dependency regeneration.
// The exporter is apps/api/tests/export_donor_identity.py at commit c0b2bbb; the
// scalar digest in testdata/identity.json pins the result for every code point.
//
//go:embed tables.json
var encodedTables []byte

type unicodeTables struct {
	Mapping      map[rune]string `json:"mapping"`
	Classes      map[rune]int    `json:"classes"`
	Compositions map[uint64]rune `json:"compositions"`
	Prohibited   [][2]rune       `json:"prohibited"`
	Right        [][2]rune       `json:"right"`
	Left         [][2]rune       `json:"left"`
	Whitespace   [][2]rune       `json:"whitespace"`
}

var tables = func() unicodeTables {
	var t unicodeTables
	if err := json.Unmarshal(encodedTables, &t); err != nil {
		panic("invalid embedded identity tables")
	}
	return t
}()
var ErrEmail = errors.New("enter a valid email address")
var localPattern = regexp.MustCompile("^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]{1,64}$")
var labelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func contains(ranges [][2]rune, r rune) bool {
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i][1] >= r })
	return i < len(ranges) && ranges[i][0] <= r
}

func ascii(value string) bool {
	for _, r := range value {
		if r > 127 {
			return false
		}
	}
	return true
}

// maxEmailRunes bounds an address before normalization, whose canonical
// reordering is quadratic in the length of a label.
const maxEmailRunes = 320

func NormalizeEmail(value string) (string, error) {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxEmailRunes {
		return "", ErrEmail
	}
	value = TrimSpace(value)
	if strings.Count(value, "@") != 1 {
		return "", ErrEmail
	}
	local, domain, _ := strings.Cut(value, "@")
	if !localPattern.MatchString(local) || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", ErrEmail
	}
	domain = strings.NewReplacer("。", ".", "．", ".", "｡", ".").Replace(domain)
	labels := strings.Split(domain, ".")
	for i, label := range labels {
		encoded, err := labelASCII(label)
		if err != nil {
			return "", ErrEmail
		}
		labels[i] = encoded
	}
	domain = strings.ToLower(strings.Join(labels, "."))
	labels = strings.Split(domain, ".")
	if len(domain) > 253 || len(labels) < 2 {
		return "", ErrEmail
	}
	for _, label := range labels {
		if !labelPattern.MatchString(label) {
			return "", ErrEmail
		}
	}
	return strings.ToLower(local) + "@" + domain, nil
}

// TrimSpace retains the legacy identity contract's whitespace set for donor-controlled names too.
func TrimSpace(value string) string {
	return strings.TrimFunc(value, func(r rune) bool { return contains(tables.Whitespace, r) })
}

func labelASCII(label string) (string, error) {
	if !ascii(label) {
		prepared, err := nameprep(label)
		if err != nil {
			return "", err
		}
		label = prepared
		if !ascii(label) {
			if strings.HasPrefix(strings.ToLower(label), "xn--") {
				return "", ErrEmail
			}
			label, err = encode("xn--", label)
			if err != nil {
				return "", ErrEmail
			}
		}
	}
	if len(label) == 0 || len(label) > 63 {
		return "", ErrEmail
	}
	return label, nil
}

func nameprep(label string) (string, error) {
	decomposed := make([]rune, 0, len(label))
	for _, r := range label {
		switch {
		case r >= 0xac00 && r < 0xd7a4:
			index := r - 0xac00
			decomposed = append(decomposed, 0x1100+index/588, 0x1161+(index%588)/28)
			if tail := index % 28; tail != 0 {
				decomposed = append(decomposed, 0x11a7+tail)
			}
		default:
			if mapped, ok := tables.Mapping[r]; ok {
				decomposed = append(decomposed, []rune(mapped)...)
			} else {
				decomposed = append(decomposed, r)
			}
		}
	}
	// Canonical ordering is stable for equal combining classes. Unlike Go's
	// stream-safe normalizer, the legacy identity contract does not insert a CGJ
	// after 30 non-starters.
	for i := 1; i < len(decomposed); i++ {
		class := tables.Classes[decomposed[i]]
		if class == 0 {
			continue
		}
		for j := i; j > 0 && tables.Classes[decomposed[j-1]] > class; j-- {
			decomposed[j], decomposed[j-1] = decomposed[j-1], decomposed[j]
		}
	}
	result := make([]rune, 0, len(decomposed))
	starter := -1
	lastClass := 0
	for _, r := range decomposed {
		class := tables.Classes[r]
		if starter >= 0 && (lastClass == 0 || lastClass < class) {
			if combined, ok := compose(result[starter], r); ok {
				result[starter] = combined
				continue
			}
		}
		if class == 0 {
			starter = len(result)
		}
		result = append(result, r)
		lastClass = class
	}
	hasRight, hasLeft := false, false
	for _, r := range result {
		if contains(tables.Prohibited, r) {
			return "", ErrEmail
		}
		hasRight = hasRight || contains(tables.Right, r)
		hasLeft = hasLeft || contains(tables.Left, r)
	}
	if hasRight && (hasLeft || !contains(tables.Right, result[0]) || !contains(tables.Right, result[len(result)-1])) {
		return "", ErrEmail
	}
	return string(result), nil
}

func compose(a, b rune) (rune, bool) {
	if a >= 0x1100 && a < 0x1113 && b >= 0x1161 && b < 0x1176 {
		return 0xac00 + (a-0x1100)*588 + (b-0x1161)*28, true
	}
	if a >= 0xac00 && a < 0xd7a4 && (a-0xac00)%28 == 0 && b > 0x11a7 && b < 0x11c3 {
		return a + b - 0x11a7, true
	}
	value, ok := tables.Compositions[(uint64(a)<<21)|uint64(b)]
	return value, ok
}
