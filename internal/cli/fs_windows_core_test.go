//go:build windows

package cli

import (
	"github.com/samekind/codexfold/internal/enroll"
	"testing"
	"time"
)

func TestWindowsCorePauseAcknowledgements(t *testing.T) {
	store := t.TempDir()
	if err := requireWindowsEnrollmentPaused(store); err != nil {
		t.Fatal("no loop configured", err)
	}
	if err := enroll.SaveProgress(enroll.ProgressPath(store), enroll.Progress{StorePath: store, Phase: enroll.PhaseDisabled, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := requireWindowsEnrollmentPaused(store); err != nil {
		t.Fatal("absent external worker must not block a fresh builtin pause", err)
	}
	policy := enroll.Control{Present: true, Enabled: false, Interval: time.Minute, StableFor: time.Hour, BatchSize: 5}
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), policy); err != nil {
		t.Fatal(err)
	}
	if err := requireWindowsEnrollmentPaused(store); err == nil {
		t.Fatal("missing configured worker acknowledgement accepted")
	}
	if err := enroll.SaveProgress(enroll.WorkerProgressPath(store), enroll.Progress{StorePath: store, Phase: enroll.PhaseReclaiming, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := requireWindowsEnrollmentPaused(store); err == nil {
		t.Fatal("active worker accepted")
	}
	if err := enroll.SaveProgress(enroll.WorkerProgressPath(store), enroll.Progress{StorePath: store, Phase: enroll.PhaseDisabled, UpdatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := requireWindowsEnrollmentPaused(store); err == nil {
		t.Fatal("stale pause accepted")
	}
	if err := enroll.SaveProgress(enroll.WorkerProgressPath(store), enroll.Progress{StorePath: store, Phase: enroll.PhaseDisabled, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := requireWindowsEnrollmentPaused(store); err != nil {
		t.Fatal(err)
	}
}
