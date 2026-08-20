package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samekind/codexfold/internal/fskitstatus"
	"github.com/samekind/codexfold/internal/vfs"
)

const managedRecoveryDeadline = 10 * time.Second

type managedReloadObservation struct {
	Sequence         uint64
	Fatal            error
	StateIssues      []vfs.SessionStateIssue
	MissingState     []string
	MissingRoute     []string
	ManagedSessions  int
	FailureStartedAt time.Time
}

func (o managedReloadObservation) Err() error {
	var faults []error
	if o.Fatal != nil {
		faults = append(faults, o.Fatal)
	}
	if len(o.StateIssues) != 0 {
		faults = append(faults, fmt.Errorf("%d managed session state issue(s)", len(o.StateIssues)))
	}
	if len(o.MissingState) != 0 {
		faults = append(faults, fmt.Errorf("managed session state metadata is temporarily unavailable for %s", strings.Join(o.MissingState, ",")))
	}
	if len(o.MissingRoute) != 0 {
		faults = append(faults, fmt.Errorf("Codex route metadata is temporarily unavailable for %s", strings.Join(o.MissingRoute, ",")))
	}
	return errors.Join(faults...)
}

func (o managedReloadObservation) reason() string {
	switch {
	case o.Fatal != nil:
		return "CodexFold could not refresh one or more managed sessions."
	case len(o.StateIssues) != 0:
		return "One or more managed session state records are temporarily unavailable."
	case len(o.MissingState) != 0:
		return "Previously managed sessions are missing from the latest storage observation."
	case len(o.MissingRoute) != 0:
		return "Codex did not return a current route for one or more managed sessions."
	default:
		return ""
	}
}

type managedStatusIncident struct {
	id      string
	started time.Time
}

type managedStatusTracker struct {
	active              *managedStatusIncident
	recovered           *managedStatusIncident
	observationSequence uint64
	publisherInstanceID string
	backendID           string
	newIncident         func() (string, error)
}

func newManagedStatusTracker() *managedStatusTracker {
	publisherInstanceID, err := newManagedPublisherInstanceID()
	if err != nil {
		publisherInstanceID = fmt.Sprintf("managed-publisher-%x", time.Now().UnixNano())
	}
	return &managedStatusTracker{
		publisherInstanceID: publisherInstanceID,
		newIncident:         newManagedIncidentID,
	}
}

func (t *managedStatusTracker) snapshot(observation managedReloadObservation, now time.Time) fskitstatus.Snapshot {
	now = now.UTC()
	managedSessions := observation.ManagedSessions
	snapshot := fskitstatus.Snapshot{
		Component:           "managed",
		State:               "healthy",
		UpdatedAt:           now.Format(time.RFC3339Nano),
		ObservationSequence: nextManagedStatusObservationSequence(t.observationSequence),
		PublisherInstanceID: t.publisherInstanceID,
		BackendID:           t.backendID,
		Summary:             "Managed sessions are available",
		ManagedSessions:     &managedSessions,
	}
	t.observationSequence = snapshot.ObservationSequence
	fault := observation.Err()
	if fault == nil {
		if t.active != nil {
			t.recovered = t.active
			t.active = nil
		}
		if t.recovered != nil {
			snapshot.IncidentID = t.recovered.id
			snapshot.RecoveryStartedAt = t.recovered.started.Format(time.RFC3339Nano)
			snapshot.ElapsedMilliseconds = max(now.Sub(t.recovered.started).Milliseconds(), 0)
		}
		return snapshot
	}
	if t.active == nil {
		t.recovered = nil
		id, err := t.newIncident()
		if err != nil {
			digest := fmt.Sprintf("managed-%x", now.UnixNano())
			id = digest
		}
		started := observation.FailureStartedAt.UTC()
		if started.IsZero() || started.After(now) {
			started = now
		}
		t.active = &managedStatusIncident{id: id, started: started}
	}
	snapshot.State = "recovering"
	snapshot.Summary = "Managed sessions are recovering"
	snapshot.Detail = fault.Error()
	snapshot.IncidentID = t.active.id
	snapshot.RecoveryStartedAt = t.active.started.Format(time.RFC3339Nano)
	snapshot.RecoveryDeadlineAt = t.active.started.Add(managedRecoveryDeadline).Format(time.RFC3339Nano)
	snapshot.ElapsedMilliseconds = max(now.Sub(t.active.started).Milliseconds(), 0)
	snapshot.Reason = observation.reason()
	snapshot.Impact = "Codex remains running. Last-known-good sessions stay mounted, but affected sessions may be temporarily unavailable."
	snapshot.Recommendations = []string{
		"Keep Codex open while CodexFold retries automatically.",
		"Check that the CodexFold storage volume is connected, writable, and has free space.",
		"Review the status details before changing or restarting any process.",
	}
	return snapshot
}

type managedStatusReporter struct {
	mu           sync.Mutex
	tracker      *managedStatusTracker
	publisher    *fskitstatus.Publisher
	publish      func(fskitstatus.Snapshot)
	lastSequence uint64
	now          func() time.Time
	mountPoint   string
	resourcePath string
}

func newManagedStatusReporter(path string, mountPoint string, resourcePath string, onError func(error)) *managedStatusReporter {
	return newManagedStatusReporterForStore(path, "", mountPoint, resourcePath, onError)
}

// newManagedStatusReporterForStore binds status causality to the configured
// store path as well as the mount and FSKit resource paths. Paths are cleaned,
// but symlinks are intentionally not resolved: recreating a store at the same
// configured path retains the same logical backend identity.
func newManagedStatusReporterForStore(path string, storePath string, mountPoint string, resourcePath string, onError func(error)) *managedStatusReporter {
	tracker := newManagedStatusTracker()
	tracker.backendID = managedBackendID(storePath, mountPoint, resourcePath)
	if path != "" {
		if previous, err := fskitstatus.Read(path); err == nil {
			tracker.resume(previous, tracker.backendID, mountPoint, resourcePath, time.Now())
		} else if !errors.Is(err, os.ErrNotExist) && onError != nil {
			onError(fmt.Errorf("read previous managed session status: %w", err))
		}
	}
	reporter := &managedStatusReporter{
		tracker: tracker, now: time.Now,
		mountPoint: mountPoint, resourcePath: resourcePath,
	}
	if path != "" {
		reporter.publisher = fskitstatus.NewPublisher(path, onError)
		reporter.publish = reporter.publisher.Publish
	}
	return reporter
}

func (t *managedStatusTracker) resume(snapshot fskitstatus.Snapshot, backendID string, mountPoint string, resourcePath string, now time.Time) {
	if snapshot.SchemaVersion != fskitstatus.SchemaVersion ||
		snapshot.Component != "managed" ||
		snapshot.BackendID != backendID ||
		snapshot.MountPoint != mountPoint ||
		snapshot.ResourcePath != resourcePath ||
		snapshot.PublisherInstanceID == "" ||
		snapshot.ObservationSequence == 0 {
		return
	}
	if snapshot.IncidentID == "" {
		return
	}
	started, err := time.Parse(time.RFC3339Nano, snapshot.RecoveryStartedAt)
	if err != nil {
		started, err = time.Parse(time.RFC3339Nano, snapshot.UpdatedAt)
	}
	if err != nil {
		return
	}
	now = now.UTC()
	if started.After(now) {
		started = now
	}
	incident := &managedStatusIncident{id: snapshot.IncidentID, started: started.UTC()}
	switch snapshot.State {
	case "healthy":
		t.recovered = incident
	case "recovering", "failed", "unavailable":
		t.active = incident
	}
}

func nextManagedStatusObservationSequence(previous uint64) uint64 {
	if previous != ^uint64(0) {
		return previous + 1
	}
	return previous
}

func (r *managedStatusReporter) Observe(observation managedReloadObservation) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if observation.Sequence != 0 && observation.Sequence <= r.lastSequence {
		return
	}
	if observation.Sequence != 0 {
		r.lastSequence = observation.Sequence
	}
	snapshot := r.tracker.snapshot(observation, r.now())
	snapshot.MountPoint = r.mountPoint
	snapshot.ResourcePath = r.resourcePath
	if r.publish != nil {
		r.publish(snapshot)
	}
}

func (r *managedStatusReporter) Close(timeout time.Duration) error {
	if r == nil || r.publisher == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.publisher.Close(ctx)
}

func newManagedIncidentID() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return "managed-" + hex.EncodeToString(token[:]), nil
}

func newManagedPublisherInstanceID() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return "managed-publisher-" + hex.EncodeToString(token[:]), nil
}

func managedBackendID(storePath string, mountPoint string, resourcePath string) string {
	identity := strings.Join([]string{
		"codexfold-managed-backend-v1",
		canonicalManagedBackendPath(storePath, "<unspecified-store>"),
		canonicalManagedBackendPath(mountPoint, "<unspecified-mount>"),
		canonicalManagedBackendPath(resourcePath, "<unspecified-resource>"),
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return "managed-backend-sha256-" + hex.EncodeToString(digest[:])
}

func canonicalManagedBackendPath(path string, unspecified string) string {
	if path == "" {
		return unspecified
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(path)
}

func missingManagedSessionIDs(known map[string]uint64, seen map[string]struct{}) []string {
	missing := make([]string, 0)
	for sessionID := range known {
		if _, exists := seen[sessionID]; !exists {
			missing = append(missing, sessionID)
		}
	}
	sort.Strings(missing)
	return missing
}

func retainedManagedSessionCount(known map[string]uint64, seen map[string]struct{}) int {
	retained := make(map[string]struct{}, len(known)+len(seen))
	for sessionID := range known {
		retained[sessionID] = struct{}{}
	}
	for sessionID := range seen {
		retained[sessionID] = struct{}{}
	}
	return len(retained)
}

func missingManagedManifestIDs(store string, states []vfs.SessionState) ([]string, error) {
	store = filepath.Clean(store)
	if !filepath.IsAbs(store) {
		return nil, errors.New("absolute managed store path is required")
	}
	root, err := os.OpenRoot(store)
	if err != nil {
		return nil, fmt.Errorf("open managed store root: %w", err)
	}
	defer root.Close()
	manifestRoot := filepath.Join(store, "manifests")
	missing := make([]string, 0)
	for _, state := range states {
		target := filepath.Clean(state.ManifestPath)
		manifestRelative, err := filepath.Rel(manifestRoot, target)
		if err != nil || manifestRelative == "." || manifestRelative == ".." || strings.HasPrefix(manifestRelative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("managed manifest for session %s is outside the canonical manifest store", state.SessionID)
		}
		storeRelative, err := filepath.Rel(store, target)
		if err != nil {
			return nil, err
		}
		info, err := root.Lstat(storeRelative)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, state.SessionID)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect managed manifest for session %s: %w", state.SessionID, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("managed manifest for session %s is not a regular file", state.SessionID)
		}
	}
	sort.Strings(missing)
	return missing, nil
}
