package uicatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const maxNesting = 64

var errNesting = errors.New("value is nested too deeply")

// object keeps the order in which keys first appeared, which decides the order
// validation errors are reported in.
type object struct {
	keys   []string
	values map[string]any
}

func (o *object) has(key string) bool { _, ok := o.values[key]; return ok }

// parse reads one JSON value, keeping object key order. A repeated key keeps
// its first position and its last value, as JSON.parse does.
func parse(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := parseValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return value, nil
}

func parseValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > maxNesting {
		return nil, errNesting
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		if text, isText := token.(string); isText && strings.ContainsRune(text, 0) {
			return nil, errors.New("string contains NUL")
		}
		return token, nil
	}
	if delim == '[' {
		items := []any{}
		for decoder.More() {
			item, err := parseValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		_, err = decoder.Token()
		return items, err
	}
	out := &object{values: map[string]any{}}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyToken.(string)
		if strings.ContainsRune(key, 0) {
			return nil, errors.New("key contains NUL")
		}
		value, err := parseValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		if !out.has(key) {
			out.keys = append(out.keys, key)
		}
		out.values[key] = value
	}
	_, err = decoder.Token()
	return out, err
}
