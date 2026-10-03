package tray

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistorySurvivesRestartAndRetainsOnlyObservedThirtyDays(t *testing.T) {
	store := t.TempDir()
	now := time.Now().UTC().Truncate(time.Minute)
	h := loadHistory(store, now)
	logical, physical, read := int64(2000), int64(800), float64(32)
	view := View{LogicalBytes: &logical, PhysicalBytes: &physical, ReadRate: &read, Health: "运行正常", DaemonState: "运行正常", ManagedState: "运行正常", Healthy: true}
	for minute := 31 * 24 * 60; minute >= 0; minute-- {
		h.archive.Samples = append(h.archive.Samples, HistorySample{T: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(), Logical: &logical, Physical: &physical})
	}
	h.record(view, nil, now)
	h.flush(now)
	if h.errorText != "" {
		t.Fatal(h.errorText)
	}
	loaded := loadHistory(store, now)
	if len(loaded.archive.Samples) == 0 || len(loaded.archive.Samples) > 2000 || loaded.archive.Samples[0].T < now.Add(-historyRetention).UnixMilli() {
		t.Fatalf("history retention/downsampling failed: %d", len(loaded.archive.Samples))
	}
	before := len(loaded.archive.Samples)
	loaded.record(view, nil, now.Add(10*time.Second))
	if len(loaded.archive.Samples) != before {
		t.Fatal("one minute created duplicate samples")
	}
	loaded.record(view, nil, now.Add(10*time.Minute))
	found := false
	for _, sample := range loaded.archive.Samples {
		if sample.T > now.Add(10*time.Second).UnixMilli() && sample.T < now.Add(10*time.Minute).UnixMilli() {
			t.Fatal("unobserved gap was filled with invented samples")
		}
		if sample.T == now.Add(10*time.Minute).UnixMilli() {
			found = true
		}
	}
	if !found {
		t.Fatal("new observation was not recorded")
	}
}

func TestIncidentsConfirmAfterTenSecondsAndRecoverAtObservation(t *testing.T) {
	now := time.Now().UTC()
	h := loadHistory(t.TempDir(), now)
	issue := &runtimeIssue{Source: "filesystem", Reason: "心跳停止", Impact: "访问待确认", Recommendations: []string{"检查状态"}}
	bad := View{Health: "需要关注", DaemonState: "状态不可用"}
	h.record(bad, issue, now)
	h.record(bad, issue, now.Add(9*time.Second))
	if len(h.archive.Incidents) != 0 {
		t.Fatal("transient incident persisted")
	}
	h.record(bad, issue, now.Add(10*time.Second))
	if len(h.archive.Incidents) != 1 || h.archive.Incidents[0].StartedAt != now.UnixMilli() {
		t.Fatal("sustained incident lost original observation")
	}
	loaded := loadHistory(filepath.Dir(filepath.Dir(h.path)), now.Add(time.Minute))
	if len(loaded.archive.Incidents) != 1 || loaded.archive.Incidents[0].RecoveredAt != nil {
		t.Fatal("active incident did not survive restart")
	}
	// A different component can still be failing when this source recovers.
	other := &runtimeIssue{Source: "managed", Reason: "管理状态中断"}
	loaded.record(View{DaemonState: "运行正常", ManagedState: "状态不可用"}, other, now.Add(time.Minute))
	if at := loaded.archive.Incidents[0].RecoveredAt; at == nil || *at != now.Add(time.Minute).UnixMilli() {
		t.Fatal("recovery was delayed behind unrelated failure")
	}
	loaded.flush(now.Add(time.Minute))
	again := loadHistory(filepath.Dir(filepath.Dir(h.path)), now.Add(2*time.Minute))
	if len(again.archive.Incidents) != 1 || again.archive.Incidents[0].RecoveredAt == nil {
		t.Fatal("recovery did not persist")
	}
}

func TestUnreadableHistoryIsPreservedAndDiagnosticsAreAggregateOnly(t *testing.T) {
	store := t.TempDir()
	path := filepath.Join(store, "enrollment", "ui-history-v1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"version":99,"session_text":"private sentinel"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	h := loadHistory(store, time.Now())
	h.flush(time.Now())
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) || h.errorText == "" {
		t.Fatal("unreadable history was overwritten")
	}
	view := NewMonitor(store).Refresh(time.Now())
	diagnostics, err := DiagnosticBytes(view)
	if err != nil || bytes.Contains(diagnostics, []byte("private sentinel")) {
		t.Fatal("diagnostics read or leaked session content")
	}
	output := filepath.Join(t.TempDir(), "diagnostics.json")
	if err := ExportDiagnostics(output, view); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(output); err != nil || info.Size() == 0 {
		t.Fatal("diagnostics were not written")
	}
}
