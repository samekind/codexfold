package enroll

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObservationStoreRoundTripsAndRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment", "observations.json")
	want := Observations{"session": {Path: "/tmp/session.jsonl", Size: 12, ModTimeUnixNano: 34, StableSinceUnixNano: 56}}
	if err := SaveObservations(path, want); err != nil {
		t.Fatalf("SaveObservations: %v", err)
	}
	got, err := LoadObservations(path)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	if got["session"] != want["session"] {
		t.Fatalf("observations = %#v, want %#v", got, want)
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"observations":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadObservations(path); err == nil {
		t.Fatal("unknown observation version should fail")
	}
}

func TestLoadObservationsReturnsEmptyWhenMissing(t *testing.T) {
	got, err := LoadObservations(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || len(got) != 0 {
		t.Fatalf("missing observations = %#v err=%v", got, err)
	}
}
