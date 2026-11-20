package payments

import (
	"errors"
	"testing"
)

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
