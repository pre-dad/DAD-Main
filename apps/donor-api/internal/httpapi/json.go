package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"dad/donor-api/internal/store"
	"dad/donor-api/internal/uicatalog"
)

type problem struct {
	Detail string                     `json:"detail"`
	Code   string                     `json:"code,omitempty"`   // stable reason for a refusal
	Errors []store.PublicationProblem `json:"errors,omitempty"` // set for a refused publication
}

// insightProblem is the 422 for a layout the UI catalog refuses, naming every reason.
type insightProblem struct {
	Detail string            `json:"detail"`
	Errors []uicatalog.Error `json:"errors"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("response encoding failed", "cause", fmt.Sprintf("%T", err))
		status = http.StatusInternalServerError
		w.Header().Set("Cache-Control", "no-store")
		data = []byte(`{"detail":"Response unavailable"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err = w.Write(append(data, '\n')); err != nil {
		// Close a truncated response without logging private response contents.
		panic(http.ErrAbortHandler)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeJSONLimit(w, r, target, maxJSONBody)
}

// decodeJSONLimit reads exactly one JSON value with no unknown fields,
// answering the request itself when the body is not JSON, too large, or
// malformed, or when any string holds NUL, which PostgreSQL text cannot store.
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	if r.ContentLength > limit {
		writeError(w, r, http.StatusRequestEntityTooLarge, "Request body is too large", nil)
		return false
	}
	if value := r.Header.Get("Content-Type"); value != "" {
		media, _, err := mime.ParseMediaType(value)
		if err != nil || (media != "application/json" && !(strings.HasPrefix(media, "application/") && strings.HasSuffix(media, "+json"))) {
			writeError(w, r, http.StatusUnsupportedMediaType, "Request body must be JSON", nil)
			return false
		}
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(target)
	if err == nil {
		if err = decoder.Decode(&struct{}{}); err == io.EOF && !containsNUL(reflect.ValueOf(target)) {
			return true
		}
	}
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		writeError(w, r, http.StatusRequestEntityTooLarge, "Request body is too large", nil)
	} else {
		invalidFields(w, r)
	}
	return false
}

// containsNUL reports whether any decoded string, or any raw JSON kept for
// later decoding, holds a NUL character.
func containsNUL(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.ContainsRune(v.String(), 0)
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && containsNUL(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() && containsNUL(v.Field(i)) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			return bytes.Contains(v.Bytes(), []byte(`\u0000`))
		}
		for i := range v.Len() {
			if containsNUL(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		for key, value := range v.Seq2() {
			if containsNUL(key) || containsNUL(value) {
				return true
			}
		}
	}
	return false
}

// requestBody is a decoded body that can check its own fields.
type requestBody interface{ valid() bool }

// decodeValid decodes and checks a request body, answering the request itself
// when either fails.
func decodeValid[T requestBody](w http.ResponseWriter, r *http.Request) (T, bool) {
	var body T
	if !decodeJSON(w, r, &body) {
		return body, false
	}
	if !body.valid() {
		invalidFields(w, r)
		return body, false
	}
	return body, true
}

// optional decodes a field that may be omitted but must not be null.
func optional[T any](raw json.RawMessage, fallback T) (T, bool) {
	if raw == nil {
		return fallback, true
	}
	var value T
	if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return value, false
	}
	return value, true
}
