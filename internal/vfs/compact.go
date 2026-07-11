package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jstar0/codexfold/internal/fold"
)

type PreparedGeneration struct {
	ManifestPath string
	Manifest     fold.Manifest
	View         *View
}

type CompactOptions struct {
	IdleFor     time.Duration
	Prepare     func(context.Context, NativeFile, uint64) (PreparedGeneration, error)
	BeforePhase func(string) error
}

type CompactResult struct {
	Generation uint64 `json:"generation"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
}

func (s *Session) Compact(ctx context.Context, options CompactOptions) (CompactResult, error) {
	if options.Prepare == nil {
		return CompactResult{}, errors.New("compact preparation function is required")
	}
	s.mu.Lock()
	if s.writerOpen {
		s.mu.Unlock()
		return CompactResult{}, errors.New("cannot compact while a writer lease is held")
	}
	state := s.state
	s.mu.Unlock()
	activePath := state.DeltaPath
	if state.BackingPath != "" {
		activePath = state.BackingPath
	}
	fingerprint, err := captureFingerprint(activePath)
	if err != nil {
		return CompactResult{}, err
	}
	if options.IdleFor > 0 && time.Since(fingerprint.ModTime) < options.IdleFor {
		return CompactResult{}, errors.New("session is not idle enough for compaction")
	}
	current, err := s.MaterializeCurrent(ctx, filepath.Join(s.directory, fmt.Sprintf(".compact-%020d.jsonl", state.Generation)), true)
	if err != nil {
		return CompactResult{}, err
	}
	defer os.Remove(current.Path)
	prepared, err := options.Prepare(ctx, current, state.Generation+1)
	if err != nil {
		return CompactResult{}, err
	}
	if prepared.View == nil || prepared.ManifestPath == "" || prepared.Manifest.Source.SHA256 != current.SHA256 || prepared.Manifest.Source.Bytes != current.Bytes {
		return CompactResult{}, errors.New("compact preparation returned incomplete generation")
	}
	if prepared.View.Size() != current.Bytes {
		return CompactResult{}, errors.New("prepared generation byte length differs from current view")
	}
	preparedDigest, err := hashView(ctx, prepared.View)
	if err != nil {
		return CompactResult{}, err
	}
	if preparedDigest != current.SHA256 {
		return CompactResult{}, errors.New("prepared generation SHA-256 differs from current view")
	}
	if err := ensureFingerprintUnchanged(activePath, fingerprint); err != nil {
		return CompactResult{}, err
	}
	newDelta := filepath.Join(s.directory, fmt.Sprintf("delta-%020d.jsonl", state.Generation+1))
	delta, err := os.OpenFile(newDelta, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return CompactResult{}, err
	}
	if err := delta.Sync(); err != nil {
		_ = delta.Close()
		return CompactResult{}, err
	}
	if err := delta.Close(); err != nil {
		return CompactResult{}, err
	}
	next := state
	next.Generation++
	next.ManifestPath = prepared.ManifestPath
	next.BaseBytes = prepared.View.Size()
	next.BaseSHA256 = current.SHA256
	next.DeltaPath = newDelta
	next.BackingPath = ""
	operationID := fmt.Sprintf("compact-%020d", state.Generation)
	if err := appendJournal(s.directory, JournalRecord{OperationID: operationID, SessionID: state.SessionID, Kind: "compact", Phase: "prepared", Candidate: next, FinalPath: newDelta, Native: current}); err != nil {
		return CompactResult{}, err
	}
	if options.BeforePhase != nil {
		if err := options.BeforePhase("after-prepare"); err != nil {
			return CompactResult{}, err
		}
	}
	if err := appendJournal(s.directory, JournalRecord{OperationID: operationID, SessionID: state.SessionID, Kind: "compact", Phase: "state-publishing", Candidate: next, FinalPath: newDelta, Native: current}); err != nil {
		return CompactResult{}, err
	}
	if options.BeforePhase != nil {
		if err := options.BeforePhase("before-state-publish"); err != nil {
			return CompactResult{}, err
		}
	}
	if err := writeSessionState(s.statePath, next); err != nil {
		return CompactResult{}, err
	}
	s.mu.Lock()
	s.state = next
	s.view = prepared.View
	s.mu.Unlock()
	if err := appendJournal(s.directory, JournalRecord{OperationID: operationID, SessionID: state.SessionID, Kind: "compact", Phase: "state-published", Candidate: next, FinalPath: newDelta, Native: current}); err != nil {
		return CompactResult{}, err
	}
	if options.BeforePhase != nil {
		if err := options.BeforePhase("after-state-publish"); err != nil {
			return CompactResult{}, err
		}
	}
	if err := appendJournal(s.directory, JournalRecord{OperationID: operationID, SessionID: state.SessionID, Kind: "compact", Phase: "complete", Candidate: next, FinalPath: newDelta, Native: current}); err != nil {
		return CompactResult{}, err
	}
	return CompactResult{Generation: next.Generation, Bytes: current.Bytes, SHA256: current.SHA256}, nil
}

func hashView(ctx context.Context, view *View) (string, error) {
	hasher := sha256.New()
	buffer := make([]byte, 1<<20)
	var offset int64
	for offset < view.Size() {
		need := len(buffer)
		if remaining := view.Size() - offset; int64(need) > remaining {
			need = int(remaining)
		}
		n, err := view.ReadAt(ctx, buffer[:need], offset)
		if n > 0 {
			_, _ = hasher.Write(buffer[:n])
			offset += int64(n)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if n == 0 {
			break
		}
	}
	if offset != view.Size() {
		return "", errors.New("prepared generation ended before its declared size")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type fileFingerprint struct {
	Bytes   int64
	ModTime time.Time
	SHA256  string
}

func captureFingerprint(path string) (fileFingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileFingerprint{}, err
	}
	current, err := hashNativePath(path)
	if err != nil {
		return fileFingerprint{}, err
	}
	return fileFingerprint{Bytes: info.Size(), ModTime: info.ModTime(), SHA256: current.SHA256}, nil
}

func ensureFingerprintUnchanged(path string, initial fileFingerprint) error {
	current, err := captureFingerprint(path)
	if err != nil {
		return err
	}
	if current.Bytes != initial.Bytes || !current.ModTime.Equal(initial.ModTime) || current.SHA256 != initial.SHA256 {
		return errors.New("active session data changed during compaction")
	}
	return nil
}
