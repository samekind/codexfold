package fskitstatus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	SchemaVersion          = 2
	maximumStatusFileBytes = 1 << 20
)

type Snapshot struct {
	SchemaVersion       int      `json:"schemaVersion"`
	Component           string   `json:"component"`
	State               string   `json:"state"`
	UpdatedAt           string   `json:"updatedAt"`
	Summary             string   `json:"summary,omitempty"`
	Detail              string   `json:"detail,omitempty"`
	MountID             string   `json:"mountID,omitempty"`
	Generation          uint64   `json:"generation,omitempty"`
	MountPoint          string   `json:"mountPoint,omitempty"`
	ResourcePath        string   `json:"resourcePath,omitempty"`
	PID                 int      `json:"pid,omitempty"`
	RecoveryStartedAt   string   `json:"recoveryStartedAt,omitempty"`
	RecoveryDeadlineAt  string   `json:"recoveryDeadlineAt,omitempty"`
	RecoveryEpochID     string   `json:"recoveryEpochID,omitempty"`
	RecoveryEpochAt     string   `json:"recoveryEpochEstablishedAt,omitempty"`
	ElapsedMilliseconds int64    `json:"elapsedMilliseconds,omitempty"`
	ObservationSequence uint64   `json:"observationSequence,omitempty"`
	PublisherInstanceID string   `json:"publisherInstanceID,omitempty"`
	BackendID           string   `json:"backendID,omitempty"`
	LastTransportError  string   `json:"lastTransportError,omitempty"`
	IncidentID          string   `json:"incidentID,omitempty"`
	Reason              string   `json:"reason,omitempty"`
	Impact              string   `json:"impact,omitempty"`
	Recommendations     []string `json:"recommendations,omitempty"`
	ManagedSessions     *int     `json:"managedSessions,omitempty"`
	LogicalBytes        *int64   `json:"logical_bytes,omitempty"`
	PhysicalBytes       *int64   `json:"physical_bytes,omitempty"`
	ReadBytesTotal      *uint64  `json:"read_bytes_total,omitempty"`
	WrittenBytesTotal   *uint64  `json:"written_bytes_total,omitempty"`
}

type Publisher struct {
	mu      sync.Mutex
	updates chan Snapshot
	done    chan struct{}
	closed  bool
}

func NewPublisher(path string, onError func(error)) *Publisher {
	writer, err := newContinuityStatusWriter(path)
	if err != nil && onError != nil {
		onError(err)
	}
	return newPublisher(path, onError, writer.Write)
}

type continuityStatusWriter struct {
	path       string
	continuity Snapshot
	hasRecord  bool
	write      func(string, Snapshot) error
}

func newContinuityStatusWriter(path string) (*continuityStatusWriter, error) {
	writer := &continuityStatusWriter{path: path, write: Write}
	record, err := Read(ContinuityPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return writer, nil
	}
	if err != nil {
		return writer, fmt.Errorf("read FSKit status continuity: %w", err)
	}
	if err := validateContinuityRecord(record); err != nil {
		return writer, fmt.Errorf("validate FSKit status continuity: %w", err)
	}
	writer.continuity = record
	writer.hasRecord = true
	return writer, nil
}

func (w *continuityStatusWriter) Write(_ string, snapshot Snapshot) error {
	if snapshot.UpdatedAt == "" {
		snapshot.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	now, err := time.Parse(time.RFC3339Nano, snapshot.UpdatedAt)
	if err != nil {
		return fmt.Errorf("parse FSKit status update time: %w", err)
	}

	record := w.continuity
	compatible := w.hasRecord && continuityMatchesSnapshot(record, snapshot)
	if !compatible {
		record = newContinuityRecord(snapshot, now, nextContinuitySequence(record.Generation))
	}
	establishedHealthyEpoch := snapshot.State == "healthy" && record.State != "healthy"
	if establishedHealthyEpoch {
		if compatible && w.hasRecord {
			record = newContinuityRecord(snapshot, now, nextContinuitySequence(record.Generation))
		}
		record.State = "healthy"
	}
	writeContinuity := !compatible || establishedHealthyEpoch

	snapshot.RecoveryEpochID = record.RecoveryEpochID
	snapshot.RecoveryEpochAt = record.RecoveryEpochAt
	write := w.write
	if write == nil {
		write = Write
	}
	if err := write(w.path, snapshot); err != nil {
		return err
	}
	if !writeContinuity {
		return nil
	}
	if err := write(ContinuityPath(w.path), record); err != nil {
		return fmt.Errorf("publish FSKit status continuity: %w", err)
	}
	w.continuity = record
	w.hasRecord = true
	return nil
}

func newContinuityRecord(snapshot Snapshot, establishedAt time.Time, sequence uint64) Snapshot {
	epochID, err := newContinuityEpochID()
	if err != nil {
		epochID = fmt.Sprintf("continuity-epoch-%x", establishedAt.UnixNano())
	}
	return Snapshot{
		SchemaVersion:       SchemaVersion,
		Component:           snapshot.Component,
		State:               "bootstrap",
		UpdatedAt:           snapshot.UpdatedAt,
		MountPoint:          snapshot.MountPoint,
		ResourcePath:        snapshot.ResourcePath,
		Generation:          sequence,
		ObservationSequence: snapshot.ObservationSequence,
		PublisherInstanceID: snapshot.PublisherInstanceID,
		BackendID:           snapshot.BackendID,
		RecoveryEpochID:     epochID,
		RecoveryEpochAt:     establishedAt.UTC().Format(time.RFC3339Nano),
	}
}

func newContinuityEpochID() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return "continuity-epoch-" + hex.EncodeToString(token[:]), nil
}

func nextContinuitySequence(previous uint64) uint64 {
	if previous == ^uint64(0) {
		return previous
	}
	return previous + 1
}

func continuityMatchesSnapshot(record Snapshot, snapshot Snapshot) bool {
	return record.Component == snapshot.Component &&
		record.BackendID == snapshot.BackendID &&
		record.MountPoint == snapshot.MountPoint &&
		record.ResourcePath == snapshot.ResourcePath
}

func validateContinuityRecord(record Snapshot) error {
	if record.SchemaVersion != SchemaVersion || (record.State != "bootstrap" && record.State != "healthy") {
		return errors.New("unsupported FSKit status continuity record")
	}
	if record.Component == "" || record.BackendID == "" || record.PublisherInstanceID == "" || record.ObservationSequence == 0 {
		return errors.New("FSKit status continuity identity is incomplete")
	}
	if !filepath.IsAbs(record.MountPoint) || !filepath.IsAbs(record.ResourcePath) {
		return errors.New("FSKit status continuity paths must be absolute")
	}
	if record.Generation == 0 || record.RecoveryEpochID == "" || record.RecoveryEpochAt == "" {
		return errors.New("FSKit status continuity epoch is incomplete")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.RecoveryEpochAt); err != nil {
		return fmt.Errorf("parse FSKit status continuity epoch time: %w", err)
	}
	return nil
}

func ContinuityPath(statusPath string) string {
	statusPath = filepath.Clean(statusPath)
	statusDirectory := filepath.Dir(statusPath)
	return filepath.Join(filepath.Dir(statusDirectory), "status-continuity", filepath.Base(statusPath))
}

func newPublisher(path string, onError func(error), write func(string, Snapshot) error) *Publisher {
	publisher := &Publisher{updates: make(chan Snapshot, 1), done: make(chan struct{})}
	go func() {
		defer close(publisher.done)
		for snapshot := range publisher.updates {
			if err := write(path, snapshot); err != nil && onError != nil {
				onError(err)
			}
		}
	}()
	return publisher
}

// Publish never waits for filesystem I/O. If the writer is behind, only the
// newest not-yet-started observation is retained.
func (p *Publisher) Publish(snapshot Snapshot) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	select {
	case p.updates <- snapshot:
		return
	default:
	}
	select {
	case <-p.updates:
	default:
	}
	p.updates <- snapshot
}

func (p *Publisher) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.updates)
	}
	p.mu.Unlock()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func Write(path string, snapshot Snapshot) error {
	if !filepath.IsAbs(path) {
		return errors.New("absolute FSKit status path is required")
	}
	snapshot.SchemaVersion = SchemaVersion
	if snapshot.UpdatedAt == "" {
		snapshot.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode FSKit status: %w", err)
	}
	payload = append(payload, '\n')
	if len(payload) > maximumStatusFileBytes {
		return errors.New("FSKit status exceeds the size limit")
	}

	directoryPath := filepath.Dir(filepath.Clean(path))
	directory, err := openAbsoluteDirectoryNoFollow(directoryPath, true)
	if err != nil {
		return fmt.Errorf("open FSKit status directory: %w", err)
	}
	defer directory.Close()
	if err := verifyDirectoryPath(directoryPath, directory); err != nil {
		return err
	}

	temporary, temporaryName, err := createTemporaryStatusFile(directory)
	if err != nil {
		return fmt.Errorf("create temporary FSKit status: %w", err)
	}
	defer directory.Remove(temporaryName)
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary FSKit status: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary FSKit status: %w", err)
	}
	temporaryInfo, err := temporary.Stat()
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("inspect temporary FSKit status: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary FSKit status: %w", err)
	}
	if err := verifyDirectoryPath(directoryPath, directory); err != nil {
		return err
	}
	targetName := filepath.Base(path)
	if err := directory.Rename(temporaryName, targetName); err != nil {
		return fmt.Errorf("publish FSKit status: %w", err)
	}
	published, err := directory.Lstat(targetName)
	if err != nil {
		return fmt.Errorf("inspect published FSKit status: %w", err)
	}
	if !published.Mode().IsRegular() || !os.SameFile(temporaryInfo, published) || published.Size() != int64(len(payload)) {
		return errors.New("published FSKit status identity is invalid")
	}
	if err := syncRootDirectory(directory); err != nil {
		return fmt.Errorf("sync FSKit status directory: %w", err)
	}
	if err := verifyDirectoryPath(directoryPath, directory); err != nil {
		return err
	}
	return nil
}

func Read(path string) (Snapshot, error) {
	if !filepath.IsAbs(path) {
		return Snapshot{}, errors.New("absolute FSKit status path is required")
	}
	path = filepath.Clean(path)
	directoryPath := filepath.Dir(path)
	directory, err := openAbsoluteDirectoryNoFollow(directoryPath, false)
	if err != nil {
		return Snapshot{}, err
	}
	defer directory.Close()
	if err := verifyDirectoryPath(directoryPath, directory); err != nil {
		return Snapshot{}, err
	}

	name := filepath.Base(path)
	info, err := directory.Lstat(name)
	if err != nil {
		return Snapshot{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maximumStatusFileBytes {
		return Snapshot{}, errors.New("FSKit status is not a bounded regular file")
	}
	file, err := directory.Open(name)
	if err != nil {
		return Snapshot{}, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return Snapshot{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return Snapshot{}, errors.New("FSKit status changed while it was opened")
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, maximumStatusFileBytes+1))
	closedErr := file.Close()
	if err := errors.Join(readErr, closedErr); err != nil {
		return Snapshot{}, err
	}
	if len(payload) > maximumStatusFileBytes {
		return Snapshot{}, errors.New("FSKit status exceeds the size limit")
	}
	after, err := directory.Lstat(name)
	if err != nil {
		return Snapshot{}, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != int64(len(payload)) || !after.ModTime().Equal(opened.ModTime()) {
		return Snapshot{}, errors.New("FSKit status changed while it was read")
	}
	if err := verifyDirectoryPath(directoryPath, directory); err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode FSKit status: %w", err)
	}
	if err := validateSnapshot(snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func validateSnapshot(snapshot Snapshot) error {
	if snapshot.SchemaVersion != 1 && snapshot.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported FSKit status schema version %d", snapshot.SchemaVersion)
	}
	if snapshot.Component == "" || snapshot.State == "" {
		return errors.New("FSKit status component and state are required")
	}
	if snapshot.UpdatedAt == "" {
		return errors.New("FSKit status update time is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, snapshot.UpdatedAt); err != nil {
		return fmt.Errorf("parse FSKit status update time: %w", err)
	}
	if snapshot.RecoveryEpochAt != "" {
		if snapshot.RecoveryEpochID == "" {
			return errors.New("FSKit recovery epoch identity is required with its timestamp")
		}
		if _, err := time.Parse(time.RFC3339Nano, snapshot.RecoveryEpochAt); err != nil {
			return fmt.Errorf("parse FSKit recovery epoch time: %w", err)
		}
	}
	if snapshot.Component != "managed" || snapshot.SchemaVersion < 2 {
		return nil
	}
	if snapshot.PublisherInstanceID == "" || snapshot.BackendID == "" || snapshot.ObservationSequence == 0 {
		return errors.New("managed FSKit status publisher, backend, and observation sequence are required")
	}
	if !filepath.IsAbs(snapshot.MountPoint) || !filepath.IsAbs(snapshot.ResourcePath) {
		return errors.New("managed FSKit status mount and resource paths must be absolute")
	}
	hasIncidentID := snapshot.IncidentID != ""
	hasIncidentSince := snapshot.RecoveryStartedAt != ""
	if hasIncidentID != hasIncidentSince {
		return errors.New("managed FSKit status incident identity and start time must appear together")
	}
	if hasIncidentSince {
		if _, err := time.Parse(time.RFC3339Nano, snapshot.RecoveryStartedAt); err != nil {
			return fmt.Errorf("parse managed FSKit recovery start time: %w", err)
		}
	}
	if (snapshot.State == "recovering" || snapshot.State == "failed" || snapshot.State == "unavailable") && !hasIncidentID {
		return errors.New("unhealthy managed FSKit status requires an incident identity")
	}
	return nil
}

func openAbsoluteDirectoryNoFollow(path string, create bool) (*os.Root, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute directory path is required")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		parentPath := filepath.Dir(path)
		parent, parentErr := openAbsoluteDirectoryNoFollow(parentPath, true)
		if parentErr != nil {
			return nil, parentErr
		}
		name := filepath.Base(path)
		if mkdirErr := parent.Mkdir(name, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
			_ = parent.Close()
			return nil, mkdirErr
		}
		if syncErr := syncRootDirectory(parent); syncErr != nil {
			_ = parent.Close()
			return nil, syncErr
		}
		_ = parent.Close()
		info, err = os.Lstat(path)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("FSKit status directory is not a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, openedErr := root.Stat(".")
	after, afterErr := os.Lstat(path)
	if err := errors.Join(openedErr, afterErr); err != nil || !os.SameFile(info, opened) || !os.SameFile(opened, after) {
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("FSKit status directory changed while it was opened")
	}
	return root, nil
}

func verifyDirectoryPath(path string, root *os.Root) error {
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return errors.New("FSKit status directory changed while it was in use")
	}
	return nil
}

func createTemporaryStatusFile(root *os.Root) (*os.File, string, error) {
	for range 16 {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, "", err
		}
		name := ".status-" + hex.EncodeToString(token[:]) + ".tmp"
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not allocate a unique temporary FSKit status file")
}

func syncRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
