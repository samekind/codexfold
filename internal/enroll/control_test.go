package enroll

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControlRoundTripAndMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment", "policy.json")
	got, err := LoadControl(path)
	if err != nil || got.Present {
		t.Fatalf("missing control = %#v err=%v", got, err)
	}

	want := Control{
		Present:      true,
		Enabled:      true,
		Interval:     30 * time.Minute,
		StableFor:    time.Hour,
		ArchivedOnly: true,
		BatchSize:    1,
	}
	if err := SaveControl(path, want); err != nil {
		t.Fatalf("SaveControl: %v", err)
	}
	got, err = LoadControl(path)
	if err != nil {
		t.Fatalf("LoadControl: %v", err)
	}
	if got != want {
		t.Fatalf("control = %#v, want %#v", got, want)
	}
}

func TestLoadControlRejectsUnknownVersionAndZeroIdleWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"enabled":true,"interval":"30m","stable_for":"1h","archived_only":true,"batch_size":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadControl(path); err == nil {
		t.Fatal("unknown control version should fail")
	}
	if err := SaveControl(path, Control{Enabled: true, Interval: time.Minute, StableFor: 0, BatchSize: 1}); err == nil {
		t.Fatal("zero idle window should fail")
	}
}

func TestLoadControlTreatsEnabledZeroIntervalAsDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"enabled":true,"interval":"0s","stable_for":"1h","archived_only":true,"batch_size":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadControl(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present || got.Enabled || got.Interval != 0 {
		t.Fatalf("zero-interval control = %#v", got)
	}
}

func TestProgressRoundTripOmitsEmptyNextCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment", "status.json")
	want := Progress{
		StorePath:    filepath.Dir(filepath.Dir(path)),
		Enabled:      true,
		Interval:     30 * time.Minute,
		StableFor:    6 * time.Hour,
		ArchivedOnly: true,
		Phase:        PhaseChecking,
		ManagedCount: 4,
		WaitingCount: 12,
		WaitingKnown: true,
		CycleTotal:   1,
		CycleDone:    0,
		ErrorKind:    "configuration",
		LastError:    "enrollment policy is invalid",
		UpdatedAt:    time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC),
	}
	if err := SaveProgress(path, want); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	got, err := LoadProgress(path)
	if err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if got.StorePath != want.StorePath || got.Phase != want.Phase || got.ManagedCount != 4 || got.WaitingCount != 12 || !got.WaitingKnown || got.ErrorKind != "configuration" || got.LastError != want.LastError || !got.NextCheckAt.IsZero() {
		t.Fatalf("progress = %#v", got)
	}
}

func TestWaitingCountCountsOnlySoonToFoldSessions(t *testing.T) {
	plan := Plan{Decisions: []Decision{
		{SessionID: "managed", Reasons: []Reason{ReasonAlreadyManaged}},
		{SessionID: "selected", Selected: true, Eligible: true},
		{SessionID: "pending", Reasons: []Reason{ReasonStabilityPending}},
		{SessionID: "busy", Reasons: []Reason{ReasonWriterActive}},
		{SessionID: "open", Reasons: []Reason{ReasonNotArchived}},
		{SessionID: "queued", Reasons: []Reason{ReasonBatchLimit}},
	}}
	if got := WaitingCount(plan); got != 3 {
		t.Fatalf("WaitingCount = %d, want 3", got)
	}
}
