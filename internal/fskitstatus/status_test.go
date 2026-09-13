package fskitstatus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWritePublishesCompleteRestrictedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status", "daemon.json")
	if err := Write(path, Snapshot{Component: "daemon", State: "healthy", Generation: 42}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if snapshot.SchemaVersion != SchemaVersion || snapshot.Component != "daemon" || snapshot.State != "healthy" || snapshot.Generation != 42 || snapshot.UpdatedAt == "" {
		t.Fatalf("status = %#v", snapshot)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("status mode = %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "daemon.json" {
		t.Fatalf("status directory entries = %#v", entries)
	}
}

func TestWriteRoundTripsStorageAccounting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status", "storage.json")
	logicalBytes := int64(44 << 30)
	physicalBytes := int64(12 << 30)
	if err := Write(path, Snapshot{
		Component: "storage", State: "healthy",
		LogicalBytes: &logicalBytes, PhysicalBytes: &physicalBytes,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LogicalBytes == nil || *snapshot.LogicalBytes != logicalBytes ||
		snapshot.PhysicalBytes == nil || *snapshot.PhysicalBytes != physicalBytes {
		t.Fatalf("storage accounting = %#v", snapshot)
	}
}

func TestWriteRoundTripsIOActivityTotals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status", "activity.json")
	readBytes := uint64(1234)
	writtenBytes := uint64(5678)
	if err := Write(path, Snapshot{
		Component: "daemon", State: "healthy",
		ReadBytesTotal: &readBytes, WrittenBytesTotal: &writtenBytes,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ReadBytesTotal == nil || *snapshot.ReadBytesTotal != readBytes ||
		snapshot.WrittenBytesTotal == nil || *snapshot.WrittenBytesTotal != writtenBytes {
		t.Fatalf("I/O activity totals = %#v", snapshot)
	}
}

func TestPublisherNeverBlocksOnStatusIOAndKeepsLatestPendingState(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	written := make(chan Snapshot, 2)
	publisher := newPublisher("/unused/status.json", nil, func(_ string, snapshot Snapshot) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		written <- snapshot
		return nil
	})
	publisher.Publish(Snapshot{Component: "daemon", State: "starting"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("publisher worker did not start")
	}
	begin := time.Now()
	publisher.Publish(Snapshot{Component: "daemon", State: "recovering"})
	publisher.Publish(Snapshot{Component: "daemon", State: "healthy"})
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("Publish blocked on status I/O for %s", elapsed)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := publisher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	close(written)
	var states []string
	for snapshot := range written {
		states = append(states, snapshot.State)
	}
	if len(states) != 2 || states[0] != "starting" || states[1] != "healthy" {
		t.Fatalf("published states = %#v", states)
	}
}

func TestPublisherReportsWriteErrorsWithoutStoppingLaterUpdates(t *testing.T) {
	errorsSeen := make(chan error, 2)
	writes := 0
	publisher := newPublisher("/unused/status.json", func(err error) { errorsSeen <- err }, func(_ string, _ Snapshot) error {
		writes++
		if writes == 1 {
			return errors.New("disk unavailable")
		}
		return nil
	})
	publisher.Publish(Snapshot{Component: "supervisor", State: "recovering"})
	time.Sleep(10 * time.Millisecond)
	publisher.Publish(Snapshot{Component: "supervisor", State: "healthy"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := publisher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatalf("writes = %d, want 2", writes)
	}
	select {
	case err := <-errorsSeen:
		if err == nil || err.Error() != "disk unavailable" {
			t.Fatalf("reported error = %v", err)
		}
	default:
		t.Fatal("publisher did not report the write error")
	}
}

func TestPublisherContinuityChangesOnlyAfterDurableHealthyPublication(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "status", "daemon.json")
	base := Snapshot{
		Component: "daemon", State: "recovering",
		MountPoint: "/mount", ResourcePath: "/resource",
		PublisherInstanceID: "publisher-a", BackendID: "backend-a",
		ObservationSequence: 1,
	}

	publisher := NewPublisher(path, nil)
	publisher.Publish(base)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := publisher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	continuity, err := Read(ContinuityPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if continuity.State != "bootstrap" || continuity.RecoveryEpochID == "" || continuity.RecoveryEpochID != first.RecoveryEpochID {
		t.Fatalf("bootstrap continuity mismatch: status=%#v continuity=%#v", first, continuity)
	}

	restarted := NewPublisher(path, nil)
	base.PublisherInstanceID = "publisher-b"
	base.ObservationSequence = 1
	restarted.Publish(base)
	if err := restarted.Close(ctx); err != nil {
		t.Fatal(err)
	}
	stillRecovering, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := Read(ContinuityPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if stillRecovering.RecoveryEpochID != continuity.RecoveryEpochID || unchanged.RecoveryEpochID != continuity.RecoveryEpochID || unchanged.Generation != continuity.Generation {
		t.Fatalf("non-healthy restart changed continuity: status=%#v before=%#v after=%#v", stillRecovering, continuity, unchanged)
	}

	healthy := NewPublisher(path, nil)
	base.State = "healthy"
	base.PublisherInstanceID = "publisher-c"
	base.ObservationSequence = 1
	healthy.Publish(base)
	if err := healthy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	healthyStatus, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	healthyContinuity, err := Read(ContinuityPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if healthyContinuity.State != "healthy" || healthyStatus.RecoveryEpochID == continuity.RecoveryEpochID || healthyStatus.RecoveryEpochID != healthyContinuity.RecoveryEpochID {
		t.Fatalf("healthy publication did not establish a new continuity epoch: status=%#v continuity=%#v", healthyStatus, healthyContinuity)
	}
}

func TestPublisherReusesHealthyContinuityEpochForLaterHeartbeats(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "status", "supervisor.json")
	base := Snapshot{
		Component: "supervisor", State: "healthy",
		MountPoint: "/mount", ResourcePath: "/resource",
		PublisherInstanceID: "publisher-a", BackendID: "backend-a",
		ObservationSequence: 10,
	}

	first := NewPublisher(path, nil)
	first.Publish(base)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	firstStatus, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	firstContinuity, err := Read(ContinuityPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if firstContinuity.State != "healthy" || firstStatus.RecoveryEpochID == "" || firstStatus.RecoveryEpochID != firstContinuity.RecoveryEpochID {
		t.Fatalf("first healthy continuity mismatch: status=%#v continuity=%#v", firstStatus, firstContinuity)
	}

	second := NewPublisher(path, nil)
	base.ObservationSequence = 11
	second.Publish(base)
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	secondStatus, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	secondContinuity, err := Read(ContinuityPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if secondStatus.RecoveryEpochID != firstStatus.RecoveryEpochID ||
		secondStatus.RecoveryEpochAt != firstStatus.RecoveryEpochAt ||
		secondContinuity.RecoveryEpochID != firstContinuity.RecoveryEpochID ||
		secondContinuity.Generation != firstContinuity.Generation {
		t.Fatalf("healthy heartbeat reminted recovery epoch: first=%#v second=%#v firstContinuity=%#v secondContinuity=%#v", firstStatus, secondStatus, firstContinuity, secondContinuity)
	}
	if secondStatus.ObservationSequence != 11 {
		t.Fatalf("status observation sequence = %d", secondStatus.ObservationSequence)
	}

	third := NewPublisher(path, nil)
	base.BackendID = "backend-b"
	base.ObservationSequence = 1
	third.Publish(base)
	if err := third.Close(ctx); err != nil {
		t.Fatal(err)
	}
	thirdStatus, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if thirdStatus.RecoveryEpochID == secondStatus.RecoveryEpochID {
		t.Fatal("backend identity change must establish a new healthy continuity epoch")
	}
}

func TestContinuityPublicationFollowsSuccessfulMainStatusPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status", "daemon.json")
	snapshot := Snapshot{
		Component: "daemon", State: "healthy",
		MountPoint: "/mount", ResourcePath: "/resource",
		PublisherInstanceID: "publisher-a", BackendID: "backend-a",
		ObservationSequence: 1,
	}

	mainFailure := errors.New("main status unavailable")
	var failedCalls []string
	failed := &continuityStatusWriter{
		path: path,
		write: func(got string, _ Snapshot) error {
			failedCalls = append(failedCalls, got)
			return mainFailure
		},
	}
	if err := failed.Write(path, snapshot); !errors.Is(err, mainFailure) {
		t.Fatalf("main publication error = %v", err)
	}
	if len(failedCalls) != 1 || failedCalls[0] != path || failed.hasRecord {
		t.Fatalf("continuity was attempted before durable main publication: calls=%#v writer=%#v", failedCalls, failed)
	}

	continuityFailure := errors.New("continuity unavailable")
	var orderedCalls []string
	partial := &continuityStatusWriter{
		path: path,
		write: func(got string, _ Snapshot) error {
			orderedCalls = append(orderedCalls, got)
			if got == ContinuityPath(path) {
				return continuityFailure
			}
			return nil
		},
	}
	if err := partial.Write(path, snapshot); !errors.Is(err, continuityFailure) {
		t.Fatalf("continuity publication error = %v", err)
	}
	if len(orderedCalls) != 2 || orderedCalls[0] != path || orderedCalls[1] != ContinuityPath(path) || partial.hasRecord {
		t.Fatalf("publication order or in-memory commit is invalid: calls=%#v writer=%#v", orderedCalls, partial)
	}
}

func TestWriteRequiresAbsolutePathAndIdentity(t *testing.T) {
	if err := Write("status.json", Snapshot{Component: "daemon", State: "healthy"}); err == nil {
		t.Fatal("relative status path was accepted")
	}
	if err := Write(filepath.Join(t.TempDir(), "status.json"), Snapshot{}); err == nil {
		t.Fatal("status without component and state was accepted")
	}
}

func TestReadRejectsNonRegularAndMalformedStatus(t *testing.T) {
	root := t.TempDir()
	statusPath := filepath.Join(root, "managed.json")
	if err := os.Symlink(filepath.Join(root, "missing.json"), statusPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(statusPath); err == nil {
		t.Fatal("status reader followed a symlink")
	}
	if err := os.Remove(statusPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(statusPath); err == nil {
		t.Fatal("status reader accepted malformed JSON")
	}
	valid := validManagedSnapshot()
	valid.IncidentID = "managed-1"
	valid.RecoveryStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := Write(statusPath, valid); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Read(statusPath)
	if err != nil || snapshot.Component != "managed" || snapshot.IncidentID != "managed-1" {
		t.Fatalf("read status = %#v err=%v", snapshot, err)
	}
}

func TestManagedSchemaV2RequiresPublisherBackendAndSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := Write(path, Snapshot{
		Component: "managed", State: "healthy",
		MountPoint: "/mount", ResourcePath: "/resource",
	}); err == nil {
		t.Fatal("managed schema v2 without causal identity was accepted")
	}
	if err := Write(path, validManagedSnapshot()); err != nil {
		t.Fatalf("write valid managed schema v2: %v", err)
	}
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != 2 || snapshot.PublisherInstanceID == "" || snapshot.BackendID == "" || snapshot.ObservationSequence != 1 {
		t.Fatalf("managed schema v2 = %#v", snapshot)
	}
}

func TestReadWriteRejectSymlinkStatusDirectory(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	statusDirectory := filepath.Join(root, "status")
	if err := os.Symlink(external, statusDirectory); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(statusDirectory, "managed.json")
	if err := Write(path, validManagedSnapshot()); err == nil {
		t.Fatal("status writer followed a symlink directory")
	}
	if _, err := os.Stat(filepath.Join(external, "managed.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status escaped through symlink directory: %v", err)
	}
	payload, err := json.Marshal(validManagedSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "managed.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("status reader followed a symlink directory")
	}
}

func TestReadWriteEnforceStatusSizeLimit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "managed.json")
	snapshot := validManagedSnapshot()
	snapshot.Detail = strings.Repeat("x", maximumStatusFileBytes)
	if err := Write(path, snapshot); err == nil {
		t.Fatal("oversized status payload was written")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maximumStatusFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("oversized status payload was read")
	}
}

func validManagedSnapshot() Snapshot {
	return Snapshot{
		Component: "managed", State: "healthy",
		MountPoint: "/mount", ResourcePath: "/resource",
		PublisherInstanceID: "managed-publisher-test",
		BackendID:           "managed-backend-test",
		ObservationSequence: 1,
	}
}
