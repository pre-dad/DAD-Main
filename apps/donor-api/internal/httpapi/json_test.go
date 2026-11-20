package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONEncodingFailureCannotAdvertiseCachedSuccess(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]interface{}{"private": make(chan int)})
	if w.Code != 500 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "private") {
		t.Fatal("invalid response was successful, cached, or exposed private fields")
	}
}

type disconnectedWriter struct{ httptest.ResponseRecorder }

func (*disconnectedWriter) Write([]byte) (int, error) { return 0, errors.New("disconnected") }

func TestJSONWriteFailureAbortsResponse(t *testing.T) {
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Error("partial response did not abort")
		}
	}()
	writeJSON(&disconnectedWriter{}, 200, map[string]bool{"ok": true})
}

func TestDecodeJSONLimit(t *testing.T) {
	type body struct {
		Name string          `json:"name"`
		Pair [2]string       `json:"pair"`
		Raw  json.RawMessage `json:"raw"`
	}
	chunked := func(s string) io.Reader { return io.NopCloser(strings.NewReader(s)) }
	for _, tc := range []struct {
		name        string
		contentType string
		body        io.Reader
		length      int64
		limit       int64
		status      int
	}{
		{"plain JSON", "application/json", chunked(`{"name":"a"}`), -1, 64, 0},
		{"no content type", "", chunked(`{"name":"a"}`), -1, 64, 0},
		{"structured suffix", "application/merge-patch+json", chunked(`{"name":"a"}`), -1, 64, 0},
		{"form body", "text/plain", chunked(`{"name":"a"}`), -1, 64, http.StatusUnsupportedMediaType},
		{"declared length over the limit", "application/json", chunked(`{"name":"a"}`), 65, 64, http.StatusRequestEntityTooLarge},
		{"chunked body over the limit", "application/json", chunked(`{"name":"` + strings.Repeat("a", 100) + `"}`), -1, 64, http.StatusRequestEntityTooLarge},
		{"unknown field", "application/json", chunked(`{"other":1}`), -1, 64, http.StatusUnprocessableEntity},
		{"trailing value", "application/json", chunked(`{"name":"a"}{}`), -1, 64, http.StatusUnprocessableEntity},
		{"NUL in a string", "application/json", chunked(`{"name":"a\u0000"}`), -1, 64, http.StatusUnprocessableEntity},
		{"NUL in an array element", "application/json", chunked(`{"pair":["a","b\u0000"]}`), -1, 64, http.StatusUnprocessableEntity},
		{"NUL in raw JSON", "application/json", chunked(`{"raw":{"k":"\u0000"}}`), -1, 64, http.StatusUnprocessableEntity},
	} {
		r := httptest.NewRequest("POST", "/", tc.body)
		r.ContentLength = tc.length
		if tc.contentType != "" {
			r.Header.Set("Content-Type", tc.contentType)
		}
		w := httptest.NewRecorder()
		var got body
		ok := decodeJSONLimit(w, r, &got, tc.limit)
		if ok != (tc.status == 0) || (!ok && w.Code != tc.status) {
			t.Errorf("%s: ok=%v status=%d; want status %d", tc.name, ok, w.Code, tc.status)
		}
	}
}
