package uicatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dad/donor-api/api"
)

const contracts = "../../../../contracts/mobile/"

func TestEmbeddedContractsMatchTheRepository(t *testing.T) {
	for name, embedded := range map[string][]byte{"ui-catalog.json": catalogJSON, "links.json": linksJSON} {
		source, err := os.ReadFile(filepath.Join(contracts, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(source, embedded) {
			t.Errorf("internal/uicatalog/%s differs from contracts/mobile/%s; copy it over", name, name)
		}
	}
	source, err := os.ReadFile("../../api/openapi.json")
	if err != nil || !bytes.Equal(source, api.OpenAPI) {
		t.Errorf("embedded OpenAPI differs from api/openapi.json: %v", err)
	}
}

func TestPreloadAndVersion(t *testing.T) {
	if err := Preload(); err != nil {
		t.Fatal(err)
	}
	if Version() != 1 {
		t.Fatalf("Version() = %d", Version())
	}
}

type vector struct {
	Name  string          `json:"name"`
	Valid bool            `json:"valid"`
	Codes []string        `json:"codes"`
	Spec  json.RawMessage `json:"spec"`
}

func TestSharedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contracts, "ui-catalog-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		CatalogVersion int      `json:"catalog_version"`
		Vectors        []vector `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.CatalogVersion != Version() {
		t.Fatalf("vectors target catalog_version %d, validator is %d", file.CatalogVersion, Version())
	}
	if len(file.Vectors) < 60 {
		t.Fatalf("only %d vectors", len(file.Vectors))
	}
	for _, v := range file.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			result, err := Validate(v.Spec)
			if err != nil {
				t.Fatal(err)
			}
			if result.OK != v.Valid {
				t.Fatalf("OK = %v, want %v (errors %+v)", result.OK, v.Valid, result.Errors)
			}
			if v.Valid {
				return
			}
			codes := make([]string, len(result.Errors))
			for i, e := range result.Errors {
				codes[i] = e.Code
			}
			if !slices.Equal(codes, v.Codes) {
				t.Fatalf("codes = %v, want %v", codes, v.Codes)
			}
		})
	}
}

func TestPartnerIsNotNavigable(t *testing.T) {
	result, err := Validate([]byte(`[{"type":"LinkButton","navigate":"/partner"}]`))
	if err != nil || result.OK || len(result.Errors) != 1 || result.Errors[0].Code != "NAV_FORBIDDEN" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestMalformedJSON(t *testing.T) {
	for _, raw := range []string{``, `[`, `[] []`, `{"a":}`, `nope`, strings.Repeat("[", maxNesting+5) + strings.Repeat("]", maxNesting+5)} {
		if _, err := Validate([]byte(raw)); !errors.Is(err, ErrNotJSON) {
			t.Errorf("Validate(%.20q) error = %v, want ErrNotJSON", raw, err)
		}
	}
}

func TestErrorsAreReportedInTheOrderOfTheCatalog(t *testing.T) {
	result, err := Validate([]byte(`[{"type":"Metric","zeta":1,"alpha":2,"format":"nope","bind":"/nope"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, e := range result.Errors {
		codes = append(codes, e.Path+" "+e.Code)
	}
	want := []string{"/0/zeta UNKNOWN_PROP", "/0/alpha UNKNOWN_PROP", "/0/bind BIND_UNRESOLVED", "/0/format BAD_FORMAT"}
	if !slices.Equal(codes, want) {
		t.Fatalf("got %v, want %v", codes, want)
	}
}

func TestRepeatedKeysKeepTheLastValue(t *testing.T) {
	result, err := Validate([]byte(`[{"type":"Heading","title":"<b>","title":"Fine"}]`))
	if err != nil || !result.OK {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestPointerParsing(t *testing.T) {
	cases := map[string][]string{
		"/a/b":   {"a", "b"},
		"/a~1b":  {"a/b"},
		"/a~0b":  {"a~b"},
		"/a~01":  {"a~1"},
		"":       nil,
		"a":      nil,
		"/":      nil,
		"/a//b":  nil,
		"/a~":    nil,
		"/a~2":   nil,
		"/~":     nil,
		"/a/b/":  nil,
		"/a~0~1": {"a~/"},
	}
	for pointer, want := range cases {
		if got := parsePointer(pointer); !slices.Equal(got, want) {
			t.Errorf("parsePointer(%q) = %v, want %v", pointer, got, want)
		}
	}
}

func TestLabelPolicy(t *testing.T) {
	cases := map[string]string{
		"Your impact":               "",
		"":                          "LABEL_EMPTY",
		" \t\u00a0\ufeff":           "LABEL_EMPTY",
		"a<b":                       "LABEL_CONTENT",
		"line\nbreak":               "LABEL_CONTENT",
		"zero\u200bwidth":           "LABEL_CONTENT",
		"see https://example.com":   "LABEL_CONTENT",
		"visit www.example.com":     "LABEL_CONTENT",
		"JaVaScRiPt:alert":          "LABEL_CONTENT",
		"[click](here)":             "LABEL_CONTENT",
		"[a]: here":                 "LABEL_CONTENT",
		"Notes: remember, ok.":      "",
		"ratio 3:2":                 "",
		"\u00e9\u00e8 caf\u00e9 ok": "",
	}
	for text, want := range cases {
		if got := labelProblem(text); got != want {
			t.Errorf("labelProblem(%q) = %q, want %q", text, got, want)
		}
	}
	if labelProblem(3.0) != "BAD_TYPE" {
		t.Error("a non-string label is BAD_TYPE")
	}
}
