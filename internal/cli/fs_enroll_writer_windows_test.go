//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/enroll"
	"golang.org/x/sys/windows"
)

func TestWindowsWriterProbeMissingRoutesDoNotBlockOtherSessions(t *testing.T) {
	home, store, _ := fsFixture(t, true)
	missing := addEnrollmentFixtureSession(t, home, "missing", 1)
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	live := addEnrollmentFixtureSession(t, home, "live", 2)
	writer, err := os.OpenFile(live, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	allowEnrollmentNamespaceReadiness(t)
	oldMount := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = oldMount })
	flags := enrollmentFlags{codexHome: home, storeDir: store, mountPoint: filepath.Join(home, "mount"), canonicalNamespace: true, stableFor: time.Nanosecond, batchSize: 1}
	first, _, err := buildEnrollmentPlan(context.Background(), flags)
	if err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveObservations(enrollmentObservationPath(store), first.Observations); err != nil {
		t.Fatal(err)
	}
	second, _, err := buildEnrollmentPlan(context.Background(), flags)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Selected) != 1 || second.Selected[0].SessionID != "session" {
		t.Fatalf("healthy session was blocked: %#v", second)
	}
	for _, id := range []string{"missing", "live"} {
		found := false
		for _, d := range second.Decisions {
			if d.SessionID != id {
				continue
			}
			want := enroll.ReasonInvalidPath
			if id == "live" {
				want = enroll.ReasonWriterActive
			}
			for _, r := range d.Reasons {
				if r == want {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("missing blocking reason for %s: %#v", id, second)
		}
	}
	if _, err := detectEnrollmentWriters(context.Background(), []codex.Session{{ID: "missing-parent", RolloutPath: filepath.Join(home, "absent", "file.jsonl")}}); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsWriterProbeStillRejectsAccessDenied(t *testing.T) {
	name := filepath.Join(t.TempDir(), "readonly.jsonl")
	if err := os.WriteFile(name, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(name, 0o600) })
	_, err := detectEnrollmentWriters(context.Background(), []codex.Session{{ID: "readonly", RolloutPath: name}})
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("access error must not be skipped: %v", err)
	}
}

func TestWindowsWriterProbeWaitsForClosedHandleAndBlocksLiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []codex.Session{{ID: "session", RolloutPath: path}}
	writers, err := detectEnrollmentWriters(context.Background(), sessions)
	if err != nil || !writers["session"] {
		_ = file.Close()
		t.Fatalf("live writer was not blocked: %v, %v", writers, err)
	}
	closed := make(chan struct{})
	go func() { time.Sleep(40 * time.Millisecond); _ = file.Close(); close(closed) }()
	writers, err = detectEnrollmentWriters(context.Background(), sessions)
	<-closed
	if err != nil || writers["session"] {
		t.Fatalf("closed backing handle was reported as an active writer: %v, %v", writers, err)
	}
}

func TestWindowsMigrationWriterProbeUsesPhysicalSources(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.jsonl")
	retained := filepath.Join(root, "retained.jsonl")
	for _, path := range []string{source, retained} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The mounted alias is owned by the migration lease at cutover and
	// cannot be exclusively opened. Native writers still hold source handles.
	session := codex.Session{ID: "session", RolloutPath: filepath.Join(root, "unavailable-mount", "rollout.jsonl")}
	active, err := probeFilesystemMigrationWriter(context.Background(), session, source, retained)
	if err != nil || active {
		t.Fatalf("physical source probe: active=%t err=%v", active, err)
	}
	file, err := os.OpenFile(source, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	active, err = probeFilesystemMigrationWriter(context.Background(), session, source, retained)
	if err != nil || !active {
		t.Fatalf("live physical writer was not blocked: active=%t err=%v", active, err)
	}
}
