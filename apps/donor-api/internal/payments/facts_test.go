package payments

import (
	"errors"
	"testing"
	"time"
)

func TestDecodeObjectRequiresAJSONObject(t *testing.T) {
	for _, raw := range []string{``, `null`, `[]`, `"text"`, `{`} {
		if _, err := DecodeObject([]byte(raw)); !errors.Is(err, ErrFacts) {
			t.Errorf("%q: got %v, want ErrFacts", raw, err)
		}
	}
	if _, err := DecodeObject([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestObjectAccessors(t *testing.T) {
	o, err := DecodeObject(
		[]byte(
			`{"n":7,"text":"x","none":null,"yes":true,"no":false,"neg":-1,"big":253402300800,"obj":{"a":1},"rows":[{"id":"a"}],"float":1.5}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Absent("none") || !o.Absent("missing") || o.Absent("n") {
		t.Error("Absent misreports null, missing or present keys")
	}
	if o.Text("text") != "x" || o.Text("n") != "" || o.Text("missing") != "" {
		t.Error("Text must return only string values")
	}
	if v, err := o.Int("n"); err != nil || v != 7 {
		t.Errorf("Int = %d, %v", v, err)
	}
	for _, key := range []string{"none", "missing", "text", "float"} {
		if _, err := o.Int(key); !errors.Is(err, ErrFacts) {
			t.Errorf("Int(%s) = %v, want ErrFacts", key, err)
		}
	}
	if v, err := o.IntDefault("missing"); err != nil || v != 0 {
		t.Errorf("IntDefault(missing) = %d, %v", v, err)
	}
	if _, err := o.IntDefault("none"); !errors.Is(err, ErrFacts) {
		t.Errorf("IntDefault(null) = %v, want ErrFacts", err)
	}
	if v, err := o.Unix("n"); err != nil || !v.Equal(time.Unix(7, 0)) {
		t.Errorf("Unix = %v, %v", v, err)
	}
	for _, key := range []string{"neg", "big"} {
		if _, err := o.Unix(key); !errors.Is(err, ErrFacts) {
			t.Errorf("Unix(%s) = %v, want ErrFacts", key, err)
		}
	}
	if !o.BoolIs("yes", true) || o.BoolIs("yes", false) || !o.BoolIs("no", false) || o.BoolIs("missing", false) {
		t.Error("BoolIs must match only the present JSON boolean")
	}
	if inner, err := o.Object("obj"); err != nil || inner["a"] == nil {
		t.Errorf("Object = %v, %v", inner, err)
	}
	if _, err := o.Object("n"); !errors.Is(err, ErrFacts) {
		t.Errorf("Object(number) = %v, want ErrFacts", err)
	}
	if rows, err := o.Array("rows"); err != nil || len(rows) != 1 || rows[0].Text("id") != "a" {
		t.Errorf("Array = %v, %v", rows, err)
	}
}

func TestObjectNullAndMissingKeys(t *testing.T) {
	o, err := DecodeObject([]byte(`{"n":null,"i":7,"list":null,"text":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := o.IntDefault("missing"); v != 0 || err != nil {
		t.Errorf("IntDefault(missing) = %d, %v; want 0, nil", v, err)
	}
	if v, err := o.IntDefault("i"); v != 7 || err != nil {
		t.Errorf("IntDefault(i) = %d, %v; want 7, nil", v, err)
	}
	if _, err := o.IntDefault("n"); !errors.Is(err, ErrFacts) {
		t.Errorf("IntDefault(null) error = %v; want ErrFacts", err)
	}
	if rows, err := o.Array("list"); len(rows) != 0 || err != nil {
		t.Errorf("Array(null) = %v, %v; want an empty list", rows, err)
	}
	if _, err := o.Array("missing"); !errors.Is(err, ErrFacts) {
		t.Errorf("Array(missing) error = %v; want ErrFacts", err)
	}
	if got := o.Text("text"); got != "" {
		t.Errorf("Text(non-string) = %q; want empty", got)
	}
	if _, err := o.Object("n"); !errors.Is(err, ErrFacts) {
		t.Errorf("Object(null) error = %v; want ErrFacts", err)
	}
}
