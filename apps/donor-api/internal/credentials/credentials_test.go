package credentials

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestLegacyCredentials(t *testing.T) {
	var fixture struct {
		Key       string `json:"fernet_key"`
		Plaintext string `json:"fernet_plaintext"`
		Token     string `json:"legacy_fernet_token"`
		Checkout  []struct {
			Visitor  *string `json:"visitor"`
			Preimage string  `json:"preimage"`
			Hash     string  `json:"hash"`
		} `json:"checkout"`
	}
	data, err := os.ReadFile("../../testdata/credentials.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	keys, err := ParseKeyring(fixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := keys.Decrypt([]byte(fixture.Token))
	if err != nil || string(plaintext) != fixture.Plaintext {
		t.Fatalf("legacy Fernet token rejected: %v", err)
	}
	raw, err := base64.URLEncoding.DecodeString(fixture.Token)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if _, err = keys.Decrypt([]byte(base64.URLEncoding.EncodeToString(raw))); !errors.Is(err, ErrInvalid) {
		t.Fatal("tampered token accepted")
	}
	wrongKey, err := ParseKeyring(base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wrongKey.Decrypt([]byte(fixture.Token)); !errors.Is(err, ErrInvalid) {
		t.Fatal("wrong key accepted")
	}
	for _, input := range [][]byte{nil, {}, []byte("another public payload")} {
		token, err := keys.Encrypt(input)
		if err != nil {
			t.Fatal(err)
		}
		got, err := keys.Decrypt(token)
		if err != nil || !bytes.Equal(got, input) {
			t.Fatalf("Fernet roundtrip failed: %v", err)
		}
	}
	if len(fixture.Checkout) != 4 {
		t.Fatal("missing checkout fixtures")
	}
	for _, tc := range fixture.Checkout {
		got, err := CheckoutPreimage("donor@example.org", 3000, "MONTHLY", tc.Visitor)
		digest := sha256.Sum256([]byte(got))
		if err != nil || got != tc.Preimage || hex.EncodeToString(digest[:]) != tc.Hash {
			t.Fatalf("legacy checkout hash mismatch: %v", err)
		}
	}
}

func testKey(fill byte) string {
	return base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func TestParseKeyringRequiresThirtyTwoURLSafeBytesInEveryPosition(t *testing.T) {
	valid := testKey(0)
	for _, value := range []string{valid, valid + "," + testKey(1), " " + valid + " , " + testKey(1) + " "} {
		if keys, err := ParseKeyring(value); err != nil || !keys.Configured() {
			t.Errorf("valid key list rejected: %v", err)
		}
	}
	for _, invalid := range []string{"", "short", base64.URLEncoding.EncodeToString(make([]byte, 31)), base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xff", 32)))} {
		for _, tc := range []struct {
			value           string
			position, count int
		}{
			{invalid, 1, 1},
			{invalid + "," + valid, 1, 2},
			{valid + "," + invalid, 2, 2},
			{valid + "," + valid + "," + invalid, 3, 3},
		} {
			keys, err := ParseKeyring(tc.value)
			keyErr, ok := errors.AsType[*KeyError](err)
			if !ok || keyErr.Position != tc.position || keyErr.Count != tc.count || !errors.Is(err, ErrInvalid) || keys.Configured() {
				t.Errorf("%q: got %v, want key %d of %d refused", tc.value, err, tc.position, tc.count)
				continue
			}
			if invalid != "" && strings.Contains(err.Error(), invalid) {
				t.Errorf("error %q repeats key material", err)
			}
		}
	}
}

func TestKeyringEncryptsWithThePrimaryAndDecryptsWithAny(t *testing.T) {
	old, err := ParseKeyring(testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := ParseKeyring(testKey(2) + "," + testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	newOnly, err := ParseKeyring(testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := old.Encrypt([]byte("written before rotation"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Decrypt(legacy); err != nil || string(got) != "written before rotation" {
		t.Fatalf("old-key token unreadable after prepending a key: %v", err)
	}
	if rotated.IsCurrent(legacy) || !old.IsCurrent(legacy) {
		t.Fatal("old-key token reported on the wrong primary")
	}
	fresh, err := rotated.Encrypt([]byte("written after rotation"))
	if err != nil {
		t.Fatal(err)
	}
	if !rotated.IsCurrent(fresh) {
		t.Fatal("new token not on the primary key")
	}
	if got, err := newOnly.Decrypt(fresh); err != nil || string(got) != "written after rotation" {
		t.Fatalf("new token needs a retired key: %v", err)
	}
	if _, err = old.Decrypt(fresh); !errors.Is(err, ErrInvalid) {
		t.Fatal("new token decrypted without the new key")
	}
	if _, err = newOnly.Decrypt(legacy); !errors.Is(err, ErrInvalid) {
		t.Fatal("old-key token decrypted after its key was removed")
	}
}

func TestZeroKeyringIsUnconfigured(t *testing.T) {
	var keys Keyring
	if keys.Configured() || keys.IsCurrent([]byte("gAAAAA")) {
		t.Fatal("zero keyring reported keys")
	}
	if _, err := keys.Encrypt([]byte("value")); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero keyring encrypted", err)
	}
	if _, err := keys.Decrypt([]byte("gAAAAA")); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero keyring decrypted", err)
	}
}

func TestQuoteASCIIMatchesJSONEnsureASCII(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "donor@example.org", `"donor@example.org"`},
		{"empty", "", `""`},
		{"quote and backslash", "a\"b\\c", `"a\"b\\c"`},
		{"named escapes", "\b\f\n\r\t", `"\b\f\n\r\t"`},
		{"other control", "\x00\x01\x1f", `"\u0000\u0001\u001f"`},
		{"slash and tilde stay", "/~", `"/~"`},
		{"DEL", "\x7f", `"\u007f"`},
		{"latin", "caf\u00e9", `"caf\u00e9"`},
		{"BMP", "\u20ac", `"\u20ac"`},
		{"astral pair", "\U0001F600", `"\ud83d\ude00"`},
		{"lowest astral", "\U00010000", `"\ud800\udc00"`},
		{"highest astral", "\U0010FFFF", `"\udbff\udfff"`},
	} {
		got, err := QuoteASCII(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s: QuoteASCII(%q) = %q, %v; want %q", tc.name, tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"\xff", "a\xc3", "\xed\xa0\x80"} {
		if got, err := QuoteASCII(in); !errors.Is(err, ErrInvalid) || got != "" {
			t.Errorf("QuoteASCII(%q) = %q, %v; want ErrInvalid", in, got, err)
		}
	}
}
