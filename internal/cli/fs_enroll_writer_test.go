package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/enroll"
)

func TestParseEnrollmentWriterSnapshotBlocksWriteAndUpdateDescriptorsOnly(t *testing.T) {
	root := t.TempDir()
	sessions := []codex.Session{
		{ID: "read", RolloutPath: filepath.Join(root, "read.jsonl")},
		{ID: "write", RolloutPath: filepath.Join(root, "write.jsonl")},
		{ID: "update", RolloutPath: filepath.Join(root, "update.jsonl")},
	}
	output := []byte("p1\nf3\nar\nn" + sessions[0].RolloutPath + "\n" +
		"f4\naw\nn" + sessions[1].RolloutPath + "\n" +
		"f5\nau\nn" + sessions[2].RolloutPath + "\n")
	writers := parseEnrollmentWriterSnapshot(output, sessions)
	if writers["read"] || !writers["write"] || !writers["update"] {
		t.Fatalf("writer snapshot = %#v", writers)
	}
}

func TestParseEnrollmentWriterSnapshotBlocksEverySessionSharingAPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.jsonl")
	sessions := []codex.Session{{ID: "first", RolloutPath: path}, {ID: "second", RolloutPath: path}}
	writers := parseEnrollmentWriterSnapshot([]byte("p1\nf3\naw\nn"+path+"\n"), sessions)
	if !writers["first"] || !writers["second"] {
		t.Fatalf("shared-path writer snapshot = %#v", writers)
	}
}

func TestEnrollmentWriterProbeFailureFailsClosed(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	oldProbe := enrollmentWriterProbe
	defer func() { enrollmentWriterProbe = oldProbe }()
	enrollmentWriterProbe = func(context.Context, []codex.Session) (map[string]bool, error) {
		return nil, context.DeadlineExceeded
	}
	_, _, err := buildEnrollmentPlan(context.Background(), enrollmentFlags{
		codexHome: home, storeDir: storeDir, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true, canary: true,
	})
	if err == nil {
		t.Fatal("writer probe failure did not stop enrollment planning")
	}
}

func TestEnrollmentPlanBlocksSessionReportedByNativeWriterProbe(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	oldProbe := enrollmentWriterProbe
	defer func() { enrollmentWriterProbe = oldProbe }()
	enrollmentWriterProbe = func(context.Context, []codex.Session) (map[string]bool, error) {
		return map[string]bool{"session": true}, nil
	}
	oldMountProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	defer func() { mountHealthProbe = oldMountProbe }()
	info, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveObservations(enrollmentObservationPath(storeDir), enroll.Observations{"session": {
		Path: nativePath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), StableSinceUnixNano: time.Now().Add(-time.Hour).UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}
	plan, _, err := buildEnrollmentPlan(context.Background(), enrollmentFlags{
		codexHome: home, storeDir: storeDir, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true,
		canary: true, stableFor: time.Nanosecond, batchSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Selected) != 0 || len(plan.Decisions) != 1 {
		t.Fatalf("writer-active plan = %#v", plan)
	}
	found := false
	for _, reason := range plan.Decisions[0].Reasons {
		found = found || reason == enroll.ReasonWriterActive
	}
	if !found {
		t.Fatalf("writer-active reason missing: %#v", plan.Decisions[0])
	}
}
