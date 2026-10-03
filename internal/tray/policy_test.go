package tray

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
)

func TestPolicyEditsPreserveCustomValuesAndRejectStaleRevision(t *testing.T) {
	store := t.TempDir()
	path := enroll.WorkerControlPath(store)
	original := enroll.Control{Enabled: true, Interval: 37 * time.Minute, StableFor: 2 * time.Hour, BatchSize: 17}
	if err := enroll.SaveControl(path, original); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(store)
	_, _, settings, _ := m.policy()
	result := m.ApplyPolicy(PolicyChange{ID: "one", Revision: settings.Revision, Field: "archived_only", Value: json.RawMessage(`true`)})
	if !result.OK {
		t.Fatal(result.Message)
	}
	got, err := enroll.LoadControl(path)
	if err != nil || !got.Enabled || !got.ArchivedOnly || got.BatchSize != 17 || got.Interval != original.Interval || got.StableFor != original.StableFor {
		t.Fatalf("custom settings changed: %#v, %v", got, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result = m.ApplyPolicy(PolicyChange{ID: "stale", Revision: settings.Revision, Field: "enabled", Value: json.RawMessage(`false`)})
	if result.OK {
		t.Fatal("stale page overwrote current settings")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected request changed policy")
	}
	_, _, settings, _ = m.policy()
	for _, value := range []string{`null`, `"true"`} {
		if result := m.ApplyPolicy(PolicyChange{Revision: settings.Revision, Field: "enabled", Value: json.RawMessage(value)}); result.OK {
			t.Fatalf("invalid value accepted: %s", value)
		}
	}
}

func TestPolicyRepairIsExplicitPausedAndDoesNotEnableBuiltinLoop(t *testing.T) {
	store := t.TempDir()
	if err := enroll.SaveControl(enroll.ControlPath(store), enroll.Control{Interval: time.Minute, StableFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveProgress(enroll.WorkerProgressPath(store), enroll.Progress{Phase: enroll.PhaseDisabled}); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(store)
	path, _, settings, _ := m.policy()
	if path != enroll.WorkerControlPath(store) || !settings.Repairable || settings.Available {
		t.Fatalf("missing worker policy fell back to builtin: %s, %#v", path, settings)
	}
	if result := m.ApplyPolicy(PolicyChange{Field: "enabled", Value: json.RawMessage(`true`)}); result.OK {
		t.Fatal("missing policy revived defaults")
	}
	if result := m.ApplyPolicy(PolicyChange{Field: "reset", Revision: settings.Revision}); !result.OK {
		t.Fatal(result.Message)
	}
	for _, path := range []string{enroll.WorkerControlPath(store), enroll.ControlPath(store)} {
		control, err := enroll.LoadControl(path)
		if err != nil || control.Enabled {
			t.Fatalf("repair enabled policy %s: %#v, %v", path, control, err)
		}
	}
	if err := os.WriteFile(enroll.WorkerControlPath(store), []byte(`broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, settings, _ = m.policy()
	if !settings.Repairable || settings.Revision == "" {
		t.Fatal("malformed file has no repair revision")
	}
	if result := m.ApplyPolicy(PolicyChange{Field: "reset", Revision: settings.Revision}); !result.OK {
		t.Fatal(result.Message)
	}
	control, err := enroll.LoadControl(enroll.WorkerControlPath(store))
	if err != nil || control.Enabled || control.BatchSize != 0 || !control.ArchivedOnly {
		t.Fatalf("unsafe repair: %#v, %v", control, err)
	}
}

func TestDashboardPolicyRequestOnlyAcceptsStructuredSettings(t *testing.T) {
	for _, message := range []string{
		`{"action":"exec","id":"one","field":"enabled","value":true}`,
		`{"action":"set-policy","id":"one","command":"calc.exe"}`,
		`{"action":"set-policy","id":"one"} {}`,
	} {
		if _, err := decodePolicyChange(message); err == nil {
			t.Fatalf("unsafe request accepted: %s", message)
		}
	}
	if _, err := decodePolicyChange(`{"action":"set-policy","id":"one","revision":"r","field":"enabled","value":false}`); err != nil {
		t.Fatal(err)
	}
}
