package enroll

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	controlVersion  = 1
	progressVersion = 1

	PhaseDisabled       = "disabled"
	PhaseIdle           = "idle"
	PhaseChecking       = "checking"
	PhaseFolding        = "folding"
	PhasePacking        = "packing"
	PhaseMigrating      = "migrating"
	PhaseReclaiming     = "reclaiming"
	PhaseWaitingReclaim = "waiting-reclaim"
)

// Control is the hot-reloaded automatic-folding policy written by the Host GUI
// and read by the filesystem serve loop. When Present is false, serve falls
// back to its process flags.
type Control struct {
	Present      bool
	Enabled      bool
	Interval     time.Duration
	StableFor    time.Duration
	ArchivedOnly bool
	BatchSize    int
	// ConfigError is populated by the serve loop when a present policy file
	// cannot be decoded or validated. A malformed policy must fail closed
	// instead of silently reviving stale process flags.
	ConfigError string
}

type controlFile struct {
	Version      int    `json:"version"`
	Enabled      bool   `json:"enabled"`
	Interval     string `json:"interval"`
	StableFor    string `json:"stable_for"`
	ArchivedOnly bool   `json:"archived_only"`
	BatchSize    int    `json:"batch_size"`
}

// Progress is a sanitized automatic-folding snapshot for the Host. It must
// not include session titles, paths, or rollout contents.
type Progress struct {
	StorePath    string
	Enabled      bool
	Interval     time.Duration
	StableFor    time.Duration
	ArchivedOnly bool
	Phase        string
	ManagedCount int
	WaitingCount int
	WaitingKnown bool
	CycleTotal   int
	CycleDone    int
	NextCheckAt  time.Time
	LastError    string
	ErrorKind    string
	UpdatedAt    time.Time
}

type progressFile struct {
	Version      int    `json:"version"`
	StorePath    string `json:"store_path"`
	Enabled      bool   `json:"enabled"`
	Interval     string `json:"interval"`
	StableFor    string `json:"stable_for"`
	ArchivedOnly bool   `json:"archived_only"`
	Phase        string `json:"phase"`
	ManagedCount int    `json:"managed_count"`
	WaitingCount int    `json:"waiting_count"`
	WaitingKnown bool   `json:"waiting_known"`
	CycleTotal   int    `json:"cycle_total"`
	CycleDone    int    `json:"cycle_done"`
	NextCheckAt  string `json:"next_check_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	ErrorKind    string `json:"error_kind,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

func ControlPath(store string) string {
	return filepath.Join(filepath.Clean(store), "enrollment", "policy.json")
}

func ProgressPath(store string) string {
	return filepath.Join(filepath.Clean(store), "enrollment", "status.json")
}

func LoadControl(path string) (Control, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Control{}, nil
	}
	if err != nil {
		return Control{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var stored controlFile
	if err := decoder.Decode(&stored); err != nil {
		return Control{}, fmt.Errorf("decode enrollment control: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Control{}, fmt.Errorf("decode enrollment control: %w", err)
	}
	if stored.Version != controlVersion {
		return Control{}, fmt.Errorf("unsupported enrollment control version %d", stored.Version)
	}
	interval, err := time.ParseDuration(stored.Interval)
	if err != nil || interval < 0 {
		return Control{}, fmt.Errorf("invalid enrollment control interval %q", stored.Interval)
	}
	stableFor, err := time.ParseDuration(stored.StableFor)
	if err != nil || stableFor <= 0 {
		return Control{}, fmt.Errorf("invalid enrollment control stable window %q", stored.StableFor)
	}
	// A zero batch is not a broken batch: it asks the cycle to size itself from
	// what it last measured. Collapsing it to one here made every cycle pay a
	// full pack rebuild to fold a single session.
	batchSize := stored.BatchSize
	if batchSize < 0 {
		batchSize = 0
	}
	return Control{
		Present:      true,
		Enabled:      stored.Enabled && interval > 0,
		Interval:     interval,
		StableFor:    stableFor,
		ArchivedOnly: stored.ArchivedOnly,
		BatchSize:    batchSize,
	}, nil
}

func SaveControl(path string, control Control) error {
	if control.Interval < 0 || control.StableFor <= 0 {
		return errors.New("enrollment control requires a non-negative interval and a positive idle window")
	}
	batchSize := control.BatchSize
	if batchSize < 0 {
		batchSize = 0
	}
	return writeAtomicJSON(path, ".policy-*.tmp", controlFile{
		Version:      controlVersion,
		Enabled:      control.Enabled,
		Interval:     control.Interval.String(),
		StableFor:    control.StableFor.String(),
		ArchivedOnly: control.ArchivedOnly,
		BatchSize:    batchSize,
	})
}

func LoadProgress(path string) (Progress, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Progress{}, nil
	}
	if err != nil {
		return Progress{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var stored progressFile
	if err := decoder.Decode(&stored); err != nil {
		return Progress{}, fmt.Errorf("decode enrollment progress: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Progress{}, fmt.Errorf("decode enrollment progress: %w", err)
	}
	if stored.Version != progressVersion {
		return Progress{}, fmt.Errorf("unsupported enrollment progress version %d", stored.Version)
	}
	progress := Progress{
		StorePath:    stored.StorePath,
		Enabled:      stored.Enabled,
		ArchivedOnly: stored.ArchivedOnly,
		Phase:        stored.Phase,
		ManagedCount: stored.ManagedCount,
		WaitingCount: stored.WaitingCount,
		WaitingKnown: stored.WaitingKnown,
		CycleTotal:   stored.CycleTotal,
		CycleDone:    stored.CycleDone,
		LastError:    stored.LastError,
		ErrorKind:    stored.ErrorKind,
	}
	if stored.Interval != "" {
		interval, err := time.ParseDuration(stored.Interval)
		if err != nil {
			return Progress{}, fmt.Errorf("invalid enrollment progress interval %q", stored.Interval)
		}
		progress.Interval = interval
	}
	if stored.StableFor != "" {
		stableFor, err := time.ParseDuration(stored.StableFor)
		if err != nil {
			return Progress{}, fmt.Errorf("invalid enrollment progress stable window %q", stored.StableFor)
		}
		progress.StableFor = stableFor
	}
	if stored.NextCheckAt != "" {
		nextCheck, err := time.Parse(time.RFC3339Nano, stored.NextCheckAt)
		if err != nil {
			return Progress{}, fmt.Errorf("invalid enrollment progress next check: %w", err)
		}
		progress.NextCheckAt = nextCheck
	}
	if stored.UpdatedAt != "" {
		updatedAt, err := time.Parse(time.RFC3339Nano, stored.UpdatedAt)
		if err != nil {
			return Progress{}, fmt.Errorf("invalid enrollment progress update time: %w", err)
		}
		progress.UpdatedAt = updatedAt
	}
	return progress, nil
}

func SaveProgress(path string, progress Progress) error {
	if progress.UpdatedAt.IsZero() {
		progress.UpdatedAt = time.Now().UTC()
	}
	file := progressFile{
		Version:      progressVersion,
		StorePath:    progress.StorePath,
		Enabled:      progress.Enabled,
		Interval:     durationString(progress.Interval),
		StableFor:    durationString(progress.StableFor),
		ArchivedOnly: progress.ArchivedOnly,
		Phase:        progress.Phase,
		ManagedCount: progress.ManagedCount,
		WaitingCount: progress.WaitingCount,
		WaitingKnown: progress.WaitingKnown,
		CycleTotal:   progress.CycleTotal,
		CycleDone:    progress.CycleDone,
		LastError:    progress.LastError,
		ErrorKind:    progress.ErrorKind,
		UpdatedAt:    progress.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if !progress.NextCheckAt.IsZero() {
		file.NextCheckAt = progress.NextCheckAt.UTC().Format(time.RFC3339Nano)
	}
	return writeAtomicJSON(path, ".status-*.tmp", file)
}

func durationString(value time.Duration) string {
	if value <= 0 {
		return ""
	}
	return value.String()
}

func writeAtomicJSON(path string, temporaryPattern string, value any) error {
	if !filepath.IsAbs(path) {
		return errors.New("enrollment control path must be absolute")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, temporaryPattern)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncObservationDirectory(directory)
}
