package tray

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fskitstatus"
)

func TestMonitorMissingHealthyStaleAndRestart(t *testing.T) {
	store := t.TempDir()
	m := NewMonitor(store)
	now := time.Now().UTC()
	if got := m.Refresh(now); got.Health != "未连接" || got.Healthy || got.DaemonState != "未连接" || got.EnrollmentState != "未连接" {
		t.Fatalf("missing: %+v", got)
	}
	write := func(component string, sequence, read uint64, instance string, at time.Time) {
		s := fskitstatus.Snapshot{Component: component, State: "healthy", UpdatedAt: at.Format(time.RFC3339Nano), PublisherInstanceID: instance, BackendID: "test", ObservationSequence: sequence, MountPoint: store, ResourcePath: store, ReadBytesTotal: &read, WrittenBytesTotal: &read}
		if err := fskitstatus.Write(filepath.Join(store, "fs", "status", component+".json"), s); err != nil {
			t.Fatal(err)
		}
	}
	write("daemon", 1, 0, "one", now)
	write("managed", 1, 0, "one", now)
	if err := enroll.SaveProgress(enroll.ProgressPath(store), enroll.Progress{StorePath: store, Enabled: true, Phase: enroll.PhaseFolding, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got := m.Refresh(now); !got.Healthy || got.DaemonState != "运行正常" || got.ManagedState != "运行正常" || got.EnrollmentState != "正在折叠" {
		t.Fatalf("healthy: %+v", got)
	}
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), enroll.Control{Enabled: true, Interval: time.Minute, StableFor: time.Hour, BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveProgress(enroll.WorkerProgressPath(store), enroll.Progress{StorePath: store, Enabled: true, Phase: enroll.PhasePacking, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got := m.Refresh(now); got.EnrollmentState != "正在打包" {
		t.Fatalf("worker status was not selected: %+v", got)
	}
	write("daemon", 2, 2048, "one", now.Add(2*time.Second))
	write("managed", 2, 0, "one", now.Add(2*time.Second))
	if got := m.Refresh(now.Add(2 * time.Second)); !strings.Contains(got.Activity, "1.0 KiB/s") {
		t.Fatalf("rate: %+v", got)
	}
	if got := m.Refresh(now.Add(20 * time.Second)); got.Healthy || got.Health != "需要关注" || got.DaemonState != "状态不可用" || got.EnrollmentState != "等待文件系统" {
		t.Fatalf("stale: %+v", got)
	}
	write("daemon", 1, 50, "two", now.Add(21*time.Second))
	write("managed", 1, 0, "two", now.Add(21*time.Second))
	if got := m.Refresh(now.Add(21 * time.Second)); !got.Healthy || got.Activity != "读取 —    写入 —" {
		t.Fatalf("restart: %+v", got)
	}
}

func TestMonitorPersistentWorkerStatesWithoutMount(t *testing.T) {
	store := t.TempDir()
	now := time.Now()
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), enroll.Control{Enabled: true, Interval: time.Minute, StableFor: time.Hour, BatchSize: 5}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []struct {
		phase           string
		enabled         bool
		lastError, want string
	}{
		{enroll.PhaseWaitingFilesystem, true, "heartbeat is stale", "等待文件系统"},
		{enroll.PhaseStopped, true, "", "后台已停止"},
		{enroll.PhaseConfigInvalid, false, "invalid policy", "设置有误"},
		{enroll.PhaseDisabled, false, "", "已关闭"},
	} {
		if err := enroll.SaveProgress(enroll.WorkerProgressPath(store), enroll.Progress{Enabled: state.enabled, Phase: state.phase, LastError: state.lastError, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if got := NewMonitor(store).Refresh(now); got.EnrollmentState != state.want {
			t.Fatalf("%s: %+v", state.phase, got)
		}
	}
}

func TestMonitorRejectsRewrittenTimestampWithoutProgress(t *testing.T) {
	store := t.TempDir()
	m := NewMonitor(store)
	now := time.Now().UTC()
	for _, at := range []time.Time{now, now.Add(20 * time.Second)} {
		for _, component := range []string{"daemon", "managed"} {
			s := fskitstatus.Snapshot{Component: component, State: "healthy", UpdatedAt: at.Format(time.RFC3339Nano), PublisherInstanceID: "one", BackendID: "one", ObservationSequence: 1, MountPoint: store, ResourcePath: store}
			if err := fskitstatus.Write(filepath.Join(store, "fs", "status", component+".json"), s); err != nil {
				t.Fatal(err)
			}
		}
		got := m.Refresh(at)
		if at.After(now) && got.Healthy {
			t.Fatal("timestamp rewrite accepted as heartbeat")
		}
	}
}
