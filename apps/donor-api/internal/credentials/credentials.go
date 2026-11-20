// Package credentials preserves the legacy credential contract: the formats
// already stored in the deployed pre-rewrite database. Secrets and hashes must
// never be included in HTTP errors or application logs.
package credentials

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fernet/fernet-go"
)

// ErrInvalid is returned for any credential that fails to parse, decrypt or
// encode; it deliberately says nothing about why.
var ErrInvalid = errors.New("invalid credential")

func key(value string) (*fernet.Key, error) {
	decoded, err := base64.URLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return nil, ErrInvalid
	}
	k := new(fernet.Key)
	copy(k[:], decoded)
	return k, nil
}

// KeyError names the unusable entry of a key list by position, counting from
// one, so a configuration error never has to repeat key material.
type KeyError struct{ Position, Count int }

func (e *KeyError) Error() string {
	return fmt.Sprintf("key %d of %d must be 32 bytes of URL-safe base64", e.Position, e.Count)
}

func (e *KeyError) Is(target error) bool { return target == ErrInvalid }

// Keyring is an ordered list of Fernet keys. The first, the primary,
// encrypts; any of them decrypts, so a new key can be prepended and data
// re-encrypted before the old key is removed. The zero Keyring is
// unconfigured: it neither encrypts nor decrypts.
type Keyring struct{ keys []*fernet.Key }

// ParseKeyring parses a comma-separated key list, primary first, refusing
// it unless every key is usable.
func ParseKeyring(value string) (Keyring, error) {
	fields := strings.Split(value, ",")
	keys := make([]*fernet.Key, len(fields))
	for i, field := range fields {
		k, err := key(strings.TrimSpace(field))
		if err != nil {
			return Keyring{}, &KeyError{Position: i + 1, Count: len(fields)}
		}
		keys[i] = k
	}
	return Keyring{keys}, nil
}

func (k Keyring) Configured() bool { return len(k.keys) > 0 }

func (k Keyring) Encrypt(value []byte) ([]byte, error) {
	if !k.Configured() {
		return nil, ErrInvalid
	}
	return fernet.EncryptAndSign(value, k.keys[0])
}

func (k Keyring) Decrypt(token []byte) ([]byte, error) {
	return decrypt(token, k.keys)
}

// IsCurrent reports whether token decrypts under the primary key. Fernet
// tokens do not name their key, so this is how re-encryption finds the
// tokens still needing an older one.
func (k Keyring) IsCurrent(token []byte) bool {
	if !k.Configured() {
		return false
	}
	_, err := decrypt(token, k.keys[:1])
	return err == nil
}

func decrypt(token []byte, keys []*fernet.Key) ([]byte, error) {
	if _, err := base64.URLEncoding.Strict().DecodeString(string(token)); err != nil {
		return nil, ErrInvalid
	}
	// Expiration comes from login/session records, not the Fernet creation timestamp.
	result := fernet.VerifyAndDecrypt(token, 0, keys)
	if result == nil {
		return nil, ErrInvalid
	}
	return result, nil
}

// QuoteASCII reproduces the legacy contract's ASCII-only JSON string escaping.
// Checkout request hashes must match those recorded by the pre-rewrite service.
func QuoteASCII(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrInvalid
	}
	output := []byte{'"'}
	appendHex := func(r rune) {
		const digits = "0123456789abcdef"
		output = append(output, '\\', 'u', digits[(r>>12)&15], digits[(r>>8)&15], digits[(r>>4)&15], digits[r&15])
	}
	for _, r := range value {
		switch r {
		case '"', '\\':
			output = append(output, '\\', byte(r))
		case '\b':
			output = append(output, '\\', 'b')
		case '\f':
			output = append(output, '\\', 'f')
		case '\n':
			output = append(output, '\\', 'n')
		case '\r':
			output = append(output, '\\', 'r')
		case '\t':
			output = append(output, '\\', 't')
		default:
			if r < 32 || r >= 127 {
				if r > 0xffff {
					r -= 0x10000
					appendHex(0xd800 + (r >> 10))
					appendHex(0xdc00 + (r & 1023))
				} else {
					appendHex(r)
				}
			} else {
				output = append(output, byte(r))
			}
		}
	}
	output = append(output, '"')
	return string(output), nil
}

// CheckoutPreimage expects the canonical email produced by the identity adapter.
// Nil visitor and empty visitor are distinct in the existing request hash contract.
func CheckoutPreimage(email string, amount int64, frequency string, visitor *string) (string, error) {
	if amount < 100 || frequency != "MONTHLY" {
		return "", ErrInvalid
	}
	quoted, err := QuoteASCII(email)
	if err != nil {
		return "", err
	}
	cookie := "null"
	if visitor != nil {
		cookie, err = QuoteASCII(*visitor)
		if err != nil {
			return "", err
		}
	}
	return "[" + quoted + "," + strconv.FormatInt(amount, 10) + ",\"" + frequency + "\"," + cookie + "]", nil
}
