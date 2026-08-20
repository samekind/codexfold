package cli

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fskitstatus"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestManagedStatusReporterSerializesTransitionAndPublication(t *testing.T) {
	start := time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)
	firstPublishStarted := make(chan struct{})
	releaseFirstPublish := make(chan struct{})
	published := make(chan fskitstatus.Snapshot, 2)
	reporter := &managedStatusReporter{
		tracker: newManagedStatusTracker(),
		now:     func() time.Time { return start },
	}
	reporter.tracker.newIncident = func() (string, error) { return "managed-serialized", nil }
	var firstPublish atomic.Bool
	reporter.publish = func(snapshot fskitstatus.Snapshot) {
		if firstPublish.CompareAndSwap(false, true) {
			close(firstPublishStarted)
			<-releaseFirstPublish
		}
		published <- snapshot
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		reporter.Observe(managedReloadObservation{ManagedSessions: 2})
	}()
	<-firstPublishStarted

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		reporter.Observe(managedReloadObservation{
			Fatal: errors.New("loader failed"), ManagedSessions: 1, FailureStartedAt: start,
		})
	}()

	select {
	case snapshot := <-published:
		t.Fatalf("newer observation published before the older transition completed: %#v", snapshot)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirstPublish)
	<-firstDone
	<-secondDone

	first := <-published
	second := <-published
	if first.State != "healthy" || second.State != "recovering" || second.IncidentID != "managed-serialized" {
		t.Fatalf("managed publications out of order: first=%#v second=%#v", first, second)
	}
	if first.PublisherInstanceID == "" || second.PublisherInstanceID != first.PublisherInstanceID || first.ObservationSequence != 1 || second.ObservationSequence != 2 {
		t.Fatalf("managed publisher epoch is not stable and monotonic: first=%#v second=%#v", first, second)
	}
}

func TestManagedStatusReporterRejectsStaleObservation(t *testing.T) {
	start := time.Date(2026, 7, 25, 13, 30, 0, 0, time.UTC)
	published := make([]fskitstatus.Snapshot, 0, 2)
	reporter := &managedStatusReporter{
		tracker: newManagedStatusTracker(),
		now:     func() time.Time { return start },
		publish: func(snapshot fskitstatus.Snapshot) { published = append(published, snapshot) },
	}
	reporter.tracker.newIncident = func() (string, error) { return "managed-newer-failure", nil }
	reporter.Observe(managedReloadObservation{
		Sequence: 2, Fatal: errors.New("loader failed"), FailureStartedAt: start,
	})
	reporter.Observe(managedReloadObservation{Sequence: 1, ManagedSessions: 2})

	if len(published) != 1 {
		t.Fatalf("stale managed observation was published: %#v", published)
	}
	if published[0].State != "recovering" || published[0].IncidentID != "managed-newer-failure" {
		t.Fatalf("newer failure was overwritten: %#v", published[0])
	}
}

func TestManagedStatusTrackerKeepsIncidentIdentityUntilMatchingRecovery(t *testing.T) {
	start := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	ids := []string{"managed-first", "managed-second"}
	tracker := newManagedStatusTracker()
	tracker.newIncident = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}

	first := tracker.snapshot(managedReloadObservation{
		Fatal: errors.New("manifest unavailable"), ManagedSessions: 3,
		FailureStartedAt: start.Add(-2 * time.Second),
	}, start)
	if first.State != "recovering" || first.IncidentID != "managed-first" || first.RecoveryStartedAt == "" || first.RecoveryDeadlineAt == "" || first.ElapsedMilliseconds != 2_000 {
		t.Fatalf("first managed status = %#v", first)
	}
	if first.ManagedSessions == nil || *first.ManagedSessions != 3 || len(first.Recommendations) < 2 {
		t.Fatalf("first managed guidance = %#v", first)
	}
	if first.ObservationSequence == 0 {
		t.Fatalf("first managed observation sequence = %#v", first)
	}
	if first.PublisherInstanceID == "" {
		t.Fatalf("first managed publisher identity = %#v", first)
	}

	ongoing := tracker.snapshot(managedReloadObservation{
		StateIssues:     []vfs.SessionStateIssue{{SessionID: "session", Err: errors.New("state unavailable")}},
		ManagedSessions: 2,
	}, start.Add(12*time.Second))
	if ongoing.State != "recovering" || ongoing.IncidentID != "managed-first" || ongoing.RecoveryStartedAt != first.RecoveryStartedAt || ongoing.ElapsedMilliseconds != 14_000 {
		t.Fatalf("ongoing managed status = %#v", ongoing)
	}
	if ongoing.ObservationSequence <= first.ObservationSequence {
		t.Fatalf("managed observation sequence did not advance: first=%d ongoing=%d", first.ObservationSequence, ongoing.ObservationSequence)
	}
	if ongoing.PublisherInstanceID != first.PublisherInstanceID {
		t.Fatalf("managed publisher identity changed within one tracker: first=%q ongoing=%q", first.PublisherInstanceID, ongoing.PublisherInstanceID)
	}

	recovered := tracker.snapshot(managedReloadObservation{ManagedSessions: 3}, start.Add(13*time.Second))
	if recovered.State != "healthy" || recovered.IncidentID != "managed-first" || recovered.RecoveryStartedAt != first.RecoveryStartedAt || recovered.ElapsedMilliseconds != 15_000 {
		t.Fatalf("managed recovery status = %#v", recovered)
	}
	persistedRecovery := tracker.snapshot(managedReloadObservation{ManagedSessions: 3}, start.Add(15*time.Second))
	if persistedRecovery.State != "healthy" || persistedRecovery.IncidentID != recovered.IncidentID || persistedRecovery.ElapsedMilliseconds != 17_000 {
		t.Fatalf("persisted managed recovery status = %#v", persistedRecovery)
	}

	next := tracker.snapshot(managedReloadObservation{MissingRoute: []string{"session"}}, start.Add(16*time.Second))
	if next.State != "recovering" || next.IncidentID != "managed-second" || next.IncidentID == recovered.IncidentID {
		t.Fatalf("next managed incident = %#v", next)
	}
}

func TestManagedStatusTrackerResumesIncidentAcrossProcessRestart(t *testing.T) {
	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	backendID := managedBackendID("/store", "/mount", "/resource")
	tracker := newManagedStatusTracker()
	tracker.backendID = backendID
	newPublisherID := tracker.publisherInstanceID
	tracker.resume(fskitstatus.Snapshot{
		SchemaVersion: fskitstatus.SchemaVersion,
		Component:     "managed", State: "recovering", IncidentID: "managed-restarted",
		RecoveryStartedAt: start.Format(time.RFC3339Nano), UpdatedAt: start.Add(time.Second).Format(time.RFC3339Nano),
		MountPoint: "/mount", ResourcePath: "/resource", ObservationSequence: 41,
		PublisherInstanceID: "managed-publisher-old", BackendID: backendID,
	}, backendID, "/mount", "/resource", start.Add(10*time.Second))
	recovered := tracker.snapshot(managedReloadObservation{ManagedSessions: 1}, start.Add(20*time.Second))
	if recovered.State != "healthy" || recovered.IncidentID != "managed-restarted" || recovered.ElapsedMilliseconds != 20_000 || recovered.ObservationSequence != 1 {
		t.Fatalf("resumed managed recovery = %#v", recovered)
	}
	if recovered.PublisherInstanceID != newPublisherID || recovered.PublisherInstanceID == "managed-publisher-old" || recovered.BackendID != backendID {
		t.Fatalf("resumed managed recovery did not start a new publisher epoch: %#v", recovered)
	}
	stillPublished := tracker.snapshot(managedReloadObservation{ManagedSessions: 1}, start.Add(22*time.Second))
	if stillPublished.IncidentID != "managed-restarted" {
		t.Fatalf("resumed recovery identity was not retained: %#v", stillPublished)
	}
}

func TestManagedStatusTrackerRejectsDifferentBackendAndClampsFutureIncident(t *testing.T) {
	start := time.Date(2026, 7, 25, 12, 30, 0, 0, time.UTC)
	oldBackendID := managedBackendID("/old-store", "/old-mount", "/old-resource")
	previous := fskitstatus.Snapshot{
		SchemaVersion: fskitstatus.SchemaVersion,
		Component:     "managed", State: "recovering", IncidentID: "managed-old-backend",
		RecoveryStartedAt: start.Add(time.Hour).Format(time.RFC3339Nano), UpdatedAt: start.Format(time.RFC3339Nano),
		MountPoint: "/old-mount", ResourcePath: "/old-resource", ObservationSequence: 91,
		PublisherInstanceID: "managed-publisher-old", BackendID: oldBackendID,
	}

	rejected := newManagedStatusTracker()
	newBackendID := managedBackendID("/new-store", "/new-mount", "/new-resource")
	rejected.backendID = newBackendID
	rejected.resume(previous, newBackendID, "/new-mount", "/new-resource", start)
	rejected.newIncident = func() (string, error) { return "managed-new-backend", nil }
	newFailure := rejected.snapshot(managedReloadObservation{Fatal: errors.New("new failure")}, start)
	if newFailure.IncidentID != "managed-new-backend" {
		t.Fatalf("different backend incident was resumed: %#v", newFailure)
	}

	resumed := newManagedStatusTracker()
	resumed.backendID = oldBackendID
	resumed.resume(previous, oldBackendID, "/old-mount", "/old-resource", start)
	healthy := resumed.snapshot(managedReloadObservation{}, start.Add(time.Second))
	if healthy.IncidentID != "managed-old-backend" || healthy.RecoveryStartedAt != start.Format(time.RFC3339Nano) {
		t.Fatalf("future resumed incident was not clamped: %#v", healthy)
	}
}

func TestManagedStatusPublisherEpochIsPerReporterAndSequenceStartsAtOne(t *testing.T) {
	first := newManagedStatusReporterForStore("", "/store", "/mount", "/resource", nil)
	second := newManagedStatusReporterForStore("", "/store", "/mount", "/resource", nil)
	if first.tracker.publisherInstanceID == "" || second.tracker.publisherInstanceID == "" {
		t.Fatal("managed reporter publisher identity is empty")
	}
	if first.tracker.publisherInstanceID == second.tracker.publisherInstanceID {
		t.Fatalf("separate managed reporters reused publisher identity %q", first.tracker.publisherInstanceID)
	}
	firstSnapshot := first.tracker.snapshot(managedReloadObservation{}, time.Unix(1, 0))
	secondSnapshot := first.tracker.snapshot(managedReloadObservation{}, time.Unix(0, 0))
	if firstSnapshot.ObservationSequence != 1 || secondSnapshot.ObservationSequence != 2 {
		t.Fatalf("managed sequence depends on wall clock: first=%d second=%d", firstSnapshot.ObservationSequence, secondSnapshot.ObservationSequence)
	}
	if firstSnapshot.PublisherInstanceID != secondSnapshot.PublisherInstanceID {
		t.Fatalf("publisher identity changed within reporter: first=%q second=%q", firstSnapshot.PublisherInstanceID, secondSnapshot.PublisherInstanceID)
	}
}

func TestManagedBackendIDBindsStoreMountAndResourcePaths(t *testing.T) {
	base := managedBackendID("/store/./primary", "/mount/./sessions", "/resource/./native")
	cleaned := managedBackendID("/store/primary", "/mount/sessions", "/resource/native")
	if base != cleaned {
		t.Fatalf("equivalent configured paths produced different backend IDs: %q != %q", base, cleaned)
	}
	for name, candidate := range map[string]string{
		"store":    managedBackendID("/store/other", "/mount/sessions", "/resource/native"),
		"mount":    managedBackendID("/store/primary", "/mount/other", "/resource/native"),
		"resource": managedBackendID("/store/primary", "/mount/sessions", "/resource/other"),
	} {
		if candidate == cleaned {
			t.Fatalf("changing %s did not change backend ID", name)
		}
	}
}

func TestManagedReporterStartsNewEpochWhenPreviousStatusIsMissingOrMalformed(t *testing.T) {
	root := t.TempDir()
	statusDirectory := filepath.Join(root, "status")
	statusPath := filepath.Join(statusDirectory, "managed.json")
	missing := newManagedStatusReporterForStore(statusPath, "/store", "/mount", "/resource", nil)
	missingSnapshot := missing.tracker.snapshot(managedReloadObservation{}, time.Unix(1, 0))
	if missingSnapshot.ObservationSequence != 1 || missingSnapshot.PublisherInstanceID == "" {
		t.Fatalf("missing previous status did not start a new epoch: %#v", missingSnapshot)
	}
	if err := missing.Close(time.Second); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(statusDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var readErr error
	malformed := newManagedStatusReporterForStore(statusPath, "/store", "/mount", "/resource", func(err error) {
		readErr = err
	})
	malformedSnapshot := malformed.tracker.snapshot(managedReloadObservation{}, time.Unix(0, 0))
	if readErr == nil {
		t.Fatal("malformed previous status was not reported")
	}
	if malformedSnapshot.ObservationSequence != 1 || malformedSnapshot.PublisherInstanceID == "" {
		t.Fatalf("malformed previous status did not start a new epoch: %#v", malformedSnapshot)
	}
	if malformedSnapshot.PublisherInstanceID == missingSnapshot.PublisherInstanceID {
		t.Fatal("new reporters reused a publisher epoch")
	}
	if err := malformed.Close(time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestManagedReloadObservationIncludesNonFatalAvailabilityFailures(t *testing.T) {
	observation := managedReloadObservation{
		StateIssues:  []vfs.SessionStateIssue{{SessionID: "broken", Err: errors.New("invalid")}},
		MissingState: []string{"missing"}, MissingRoute: []string{"unrouted"},
	}
	if err := observation.Err(); err == nil {
		t.Fatal("state and metadata availability failures were treated as healthy")
	}
	if err := (managedReloadObservation{}).Err(); err != nil {
		t.Fatalf("empty managed observation = %v", err)
	}
}

func TestMissingManagedSessionIDsIsSortedAndDoesNotMutateKnownState(t *testing.T) {
	known := map[string]uint64{"z": 3, "present": 2, "a": 1}
	missing := missingManagedSessionIDs(known, map[string]struct{}{"present": {}})
	if len(missing) != 2 || missing[0] != "a" || missing[1] != "z" {
		t.Fatalf("missing managed sessions = %#v", missing)
	}
	if len(known) != 3 || known["present"] != 2 {
		t.Fatalf("known state was mutated: %#v", known)
	}
}

func TestMissingManagedManifestIDsUsesStoreAnchoredTraversal(t *testing.T) {
	store := t.TempDir()
	manifestRoot := filepath.Join(store, "manifests")
	if err := os.MkdirAll(manifestRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	states := []vfs.SessionState{
		{SessionID: "present", ManifestPath: filepath.Join(manifestRoot, "present.json")},
		{SessionID: "missing", ManifestPath: filepath.Join(manifestRoot, "missing.json")},
	}
	if err := os.WriteFile(states[0].ManifestPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing, err := missingManagedManifestIDs(store, states)
	if err != nil || len(missing) != 1 || missing[0] != "missing" {
		t.Fatalf("missing managed manifests = %#v err=%v", missing, err)
	}

	external := t.TempDir()
	if err := os.RemoveAll(manifestRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, manifestRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "present.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := missingManagedManifestIDs(store, states[:1]); err == nil {
		t.Fatal("managed manifest discovery followed an intermediate symlink outside the store")
	}
}
