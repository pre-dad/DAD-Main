package main

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedZonesMatchGenerator(t *testing.T) {
	want, err := render()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../zones.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("zones.json is stale; run go generate ./internal/geo")
	}
}
