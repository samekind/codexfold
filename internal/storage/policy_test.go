package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLimitsUsesBoundedDefaultsAndAllowsStoreOverride(t *testing.T) {
	store := t.TempDir()
	defaults, err := LoadLimits(store)
	if err != nil {
		t.Fatalf("LoadLimits defaults: %v", err)
	}
	if defaults.MaxPhysicalBytes <= 0 || defaults.MaxTemporaryBytes <= 0 || defaults.FreeSpaceReserveBytes <= 0 {
		t.Fatalf("default limits are not hard bounds: %#v", defaults)
	}

	want := Limits{MaxPhysicalBytes: 900, MaxTemporaryBytes: 80, FreeSpaceReserveBytes: 70}
	data, err := json.Marshal(map[string]any{"version": 1, "limits": want})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, PolicyFilename), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadLimits(store)
	if err != nil {
		t.Fatalf("LoadLimits override: %v", err)
	}
	if got != want {
		t.Fatalf("limits = %#v, want %#v", got, want)
	}
}

func TestLoadLimitsRejectsUnboundedOrUnknownPolicy(t *testing.T) {
	tests := []string{
		`{"version":2,"limits":{"max_physical_bytes":1,"max_temporary_bytes":1,"free_space_reserve_bytes":1}}`,
		`{"version":1,"limits":{"max_physical_bytes":0,"max_temporary_bytes":1,"free_space_reserve_bytes":1}}`,
		`{"version":1,"limits":{"max_physical_bytes":1,"max_temporary_bytes":1,"free_space_reserve_bytes":1},"extra":true}`,
	}
	for index, data := range tests {
		store := t.TempDir()
		if err := os.WriteFile(filepath.Join(store, PolicyFilename), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadLimits(store); err == nil {
			t.Fatalf("policy %d should fail", index)
		}
	}
}
