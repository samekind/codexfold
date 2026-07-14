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
	"sync"
	"time"

	"github.com/jstar0/codexfold/internal/fold"
)

type SessionOptions struct {
	Root           string
	ManifestPath   string
	Manifest       fold.Manifest
	Reader         ObjectReader
	NativeSnapshot NativeFile
	BeforeCOWPhase func(string) error
}

type Session struct {
	mu             sync.Mutex
	state          SessionState
	statePath      string
	directory      string
	view           *View
	readerLeases   map[uint64]int
	writerOpen     bool
	beforeCOWPhase func(string) error
}

type VisibleInfo struct {
	Size       int64
	ModTime    time.Time
	Generation uint64
}

var ErrWriterBusy = errors.New("session writer lease is already held")

func OpenSession(ctx context.Context, options SessionOptions) (*Session, error) {
	session, _, err := openSession(ctx, options, false)
	return session, err
}

func OpenSessionWithWriter(ctx context.Context, options SessionOptions) (*Session, *WriteHandle, error) {
	return openSession(ctx, options, true)
}

func openSession(ctx context.Context, options SessionOptions, reserveWriter bool) (*Session, *WriteHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if options.Root == "" || options.ManifestPath == "" || !safeSessionID(options.Manifest.Session.ID) {
		return nil, nil, errors.New("session root, manifest path, and safe session ID are required")
	}
	view, err := NewView(options.Manifest, options.Reader)
	if err != nil {
		return nil, nil, err
	}
	directory := filepath.Join(options.Root, "fs", "sessions", options.Manifest.Session.ID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create virtual session directory: %w", err)
	}
	leasePath := filepath.Join(directory, "writer.lease")
	var reservedLease *os.File
	if reserveWriter {
		reservedLease, err = acquireWriterLease(leasePath)
		if err != nil {
			return nil, nil, err
		}
	} else if err := cleanupStaleWriterLease(leasePath); err != nil {
		return nil, nil, err
	}
	cleanupReservedLease := func() {
		if reservedLease == nil {
			return
		}
		_ = unlockWriterFile(reservedLease)
		_ = reservedLease.Close()
	}
	statePath := filepath.Join(directory, "state.json")
	state, err := loadSessionState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		if err := verifyNativeFile(options.NativeSnapshot); err != nil {
			cleanupReservedLease()
			return nil, nil, err
		}
		deltaPath := filepath.Join(directory, "delta.jsonl")
		delta, err := os.OpenFile(deltaPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			cleanupReservedLease()
			return nil, nil, fmt.Errorf("create session delta: %w", err)
		}
		if err := delta.Sync(); err != nil {
			_ = delta.Close()
			cleanupReservedLease()
			return nil, nil, fmt.Errorf("sync session delta: %w", err)
		}
		if err := delta.Close(); err != nil {
			cleanupReservedLease()
			return nil, nil, fmt.Errorf("close session delta: %w", err)
		}
		state = SessionState{Version: sessionStateVersion, SessionID: options.Manifest.Session.ID, Generation: 1, ManifestPath: filepath.Clean(options.ManifestPath), BaseBytes: view.Size(), BaseSHA256: options.Manifest.Source.SHA256, DeltaPath: deltaPath, NativeSnapshot: options.NativeSnapshot}
		if err := writeSessionState(statePath, state); err != nil {
			cleanupReservedLease()
			return nil, nil, err
		}
	} else if err != nil {
		cleanupReservedLease()
		return nil, nil, err
	} else {
		if state.SessionID != options.Manifest.Session.ID || state.ManifestPath != filepath.Clean(options.ManifestPath) || state.BaseBytes != view.Size() || state.BaseSHA256 != options.Manifest.Source.SHA256 || state.NativeSnapshot != options.NativeSnapshot {
			cleanupReservedLease()
			return nil, nil, errors.New("persisted session state does not match the requested manifest")
		}
		if !pathWithin(directory, state.DeltaPath) || (state.BackingPath != "" && !pathWithin(directory, state.BackingPath)) {
			cleanupReservedLease()
			return nil, nil, errors.New("persisted session state contains an unsafe data path")
		}
		if _, err := os.Stat(state.DeltaPath); err != nil {
			cleanupReservedLease()
			return nil, nil, fmt.Errorf("stat session delta: %w", err)
		}
		if state.BackingPath != "" {
			if _, err := os.Stat(state.BackingPath); err != nil {
				cleanupReservedLease()
				return nil, nil, fmt.Errorf("stat session backing: %w", err)
			}
		}
	}
	session := &Session{state: state, statePath: statePath, directory: directory, view: view, readerLeases: make(map[uint64]int), beforeCOWPhase: options.BeforeCOWPhase}
	var writer *WriteHandle
	if reservedLease != nil {
		session.writerOpen = true
		writer = &WriteHandle{session: session, leasePath: leasePath, lease: reservedLease}
	}
	if err := session.recover(ctx); err != nil {
		if writer != nil {
			_ = writer.Close()
		}
		return nil, nil, err
	}
	if recovered, err := loadSessionState(statePath); err == nil {
		session.state = recovered
	}
	return session, writer, nil
}

func (s *Session) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) VisibleInfo() (VisibleInfo, error) {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	path := state.DeltaPath
	if state.BackingPath != "" {
		path = state.BackingPath
	}
	info, err := os.Stat(path)
	if err != nil {
		return VisibleInfo{}, err
	}
	size := info.Size()
	if state.BackingPath == "" {
		size += state.BaseBytes
	}
	return VisibleInfo{Size: size, ModTime: info.ModTime(), Generation: state.Generation}, nil
}

func (s *Session) OpenReader() (*ReadHandle, error) {
	s.mu.Lock()
	state := s.state
	view := s.view
	s.readerLeases[state.Generation]++
	s.mu.Unlock()

	handle := &ReadHandle{session: s, generation: state.Generation, base: view, baseBytes: state.BaseBytes}
	var path string
	if state.BackingPath != "" {
		path = state.BackingPath
		handle.backing = true
	} else {
		path = state.DeltaPath
	}
	file, err := os.Open(path)
	if err != nil {
		s.releaseReader(state.Generation)
		return nil, fmt.Errorf("open session reader file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		s.releaseReader(state.Generation)
		return nil, fmt.Errorf("stat session reader file: %w", err)
	}
	handle.file = file
	if handle.backing {
		handle.size = info.Size()
	} else {
		handle.deltaBytes = info.Size()
		handle.size = state.BaseBytes + info.Size()
	}
	return handle, nil
}

func (s *Session) releaseReader(generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readerLeases[generation] <= 1 {
		delete(s.readerLeases, generation)
	} else {
		s.readerLeases[generation]--
	}
}

func (s *Session) OpenWriter() (*WriteHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerOpen {
		return nil, ErrWriterBusy
	}
	leasePath := filepath.Join(s.directory, "writer.lease")
	lease, err := acquireWriterLease(leasePath)
	if err != nil {
		return nil, err
	}
	s.writerOpen = true
	return &WriteHandle{session: s, leasePath: leasePath, lease: lease}, nil
}

func acquireWriterLease(leasePath string) (*os.File, error) {
	lease, err := os.OpenFile(leasePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create writer lease: %w", err)
	}
	locked, err := tryLockWriterFile(lease)
	if err != nil {
		_ = lease.Close()
		return nil, err
	}
	if !locked {
		_ = lease.Close()
		return nil, ErrWriterBusy
	}
	if err := lease.Truncate(0); err != nil {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(lease, "%d\n", os.Getpid()); err != nil {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
		return nil, fmt.Errorf("write writer lease: %w", err)
	}
	if err := lease.Sync(); err != nil {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
		return nil, fmt.Errorf("sync writer lease: %w", err)
	}
	return lease, nil
}

func (s *Session) ensureBacking(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.state.BackingPath != "" {
		path := s.state.BackingPath
		s.mu.Unlock()
		return path, nil
	}
	currentGeneration := s.state.Generation
	s.mu.Unlock()

	reader, err := s.OpenReader()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	temporary, err := os.CreateTemp(s.directory, ".backing-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary backing: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	sourceHash := sha256.New()
	buffer := make([]byte, 1<<20)
	var offset int64
	for offset < reader.Size() {
		if err := ctx.Err(); err != nil {
			_ = temporary.Close()
			return "", err
		}
		need := len(buffer)
		if remaining := reader.Size() - offset; int64(need) > remaining {
			need = int(remaining)
		}
		n, readErr := reader.ReadAt(ctx, buffer[:need], offset)
		if n > 0 {
			_, _ = sourceHash.Write(buffer[:n])
			if _, err := temporary.Write(buffer[:n]); err != nil {
				_ = temporary.Close()
				return "", fmt.Errorf("write temporary backing: %w", err)
			}
			offset += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			_ = temporary.Close()
			return "", readErr
		}
		if n == 0 {
			break
		}
	}
	if offset != reader.Size() {
		_ = temporary.Close()
		return "", errors.New("temporary backing source read ended early")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync temporary backing: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary backing: %w", err)
	}
	verified, err := hashNativePath(temporaryPath)
	if err != nil {
		return "", err
	}
	if verified.Bytes != reader.Size() || verified.SHA256 != hex.EncodeToString(sourceHash.Sum(nil)) {
		return "", errors.New("temporary backing verification failed")
	}
	operationID := fmt.Sprintf("cow-%020d", currentGeneration)
	if err := appendJournal(s.directory, JournalRecord{OperationID: operationID, SessionID: s.state.SessionID, Kind: "copy-on-write", Phase: "data-synced", TempPath: temporaryPath, Native: verified}); err != nil {
		return "", err
	}
	if s.beforeCOWPhase != nil {
		if err := s.beforeCOWPhase("before-publish"); err != nil {
			return "", err
		}
	}
	backingPath := filepath.Join(s.directory, fmt.Sprintf("backing-%020d.jsonl", currentGeneration+1))
	if err := replaceStateFile(temporaryPath, backingPath); err != nil {
		return "", fmt.Errorf("publish session backing: %w", err)
	}
	if err := syncStateDirectory(s.directory); err != nil {
		return "", err
	}
	candidate := s.state
	candidate.Generation = currentGeneration + 1
	candidate.BackingPath = backingPath
	if err := appendJournal(s.directory, JournalRecord{OperationID: operationID, SessionID: s.state.SessionID, Kind: "copy-on-write", Phase: "after-file-publish", Candidate: candidate, FinalPath: backingPath, Native: verified}); err != nil {
		return "", err
	}
	if s.beforeCOWPhase != nil {
		if err := s.beforeCOWPhase("after-file-publish"); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Generation != currentGeneration || s.state.BackingPath != "" {
		return "", errors.New("session generation changed during copy-on-write")
	}
	next := s.state
	next.Generation++
	next.BackingPath = backingPath
	if err := writeSessionState(s.statePath, next); err != nil {
		return "", err
	}
	s.state = next
	if err := appendJournal(s.directory, JournalRecord{OperationID: operationID, SessionID: next.SessionID, Kind: "copy-on-write", Phase: "state-published", Candidate: next, FinalPath: backingPath, Native: verified}); err != nil {
		return "", err
	}
	if err := appendJournal(s.directory, JournalRecord{OperationID: operationID, SessionID: next.SessionID, Kind: "copy-on-write", Phase: "complete", Candidate: next, FinalPath: backingPath, Native: verified}); err != nil {
		return "", err
	}
	return backingPath, nil
}

func (s *Session) MaterializeCurrent(ctx context.Context, target string, overwrite bool) (NativeFile, error) {
	if target == "" {
		return NativeFile{}, errors.New("materialize target is required")
	}
	if !overwrite {
		if _, err := os.Stat(target); err == nil {
			return NativeFile{}, fmt.Errorf("materialize target already exists: %s", target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return NativeFile{}, err
		}
	}
	reader, err := s.OpenReader()
	if err != nil {
		return NativeFile{}, err
	}
	defer reader.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return NativeFile{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".materialize-*.tmp")
	if err != nil {
		return NativeFile{}, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return NativeFile{}, err
	}
	hasher := sha256.New()
	buffer := make([]byte, 1<<20)
	var offset int64
	for offset < reader.Size() {
		if err := ctx.Err(); err != nil {
			_ = temporary.Close()
			return NativeFile{}, err
		}
		need := len(buffer)
		if remaining := reader.Size() - offset; int64(need) > remaining {
			need = int(remaining)
		}
		n, readErr := reader.ReadAt(ctx, buffer[:need], offset)
		if n > 0 {
			_, _ = hasher.Write(buffer[:n])
			if _, err := temporary.Write(buffer[:n]); err != nil {
				_ = temporary.Close()
				return NativeFile{}, err
			}
			offset += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			_ = temporary.Close()
			return NativeFile{}, readErr
		}
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return NativeFile{}, err
	}
	if err := temporary.Close(); err != nil {
		return NativeFile{}, err
	}
	if overwrite {
		if err := replaceStateFile(temporaryPath, target); err != nil {
			return NativeFile{}, err
		}
	} else if err := os.Rename(temporaryPath, target); err != nil {
		return NativeFile{}, err
	}
	if err := syncStateDirectory(filepath.Dir(target)); err != nil {
		return NativeFile{}, err
	}
	expected := NativeFile{Path: target, Bytes: offset, SHA256: hex.EncodeToString(hasher.Sum(nil))}
	verified, err := hashNativePath(target)
	if err != nil {
		return NativeFile{}, err
	}
	if verified.Bytes != expected.Bytes || verified.SHA256 != expected.SHA256 {
		return NativeFile{}, errors.New("materialized current session verification failed")
	}
	return expected, nil
}

func hashNativePath(path string) (NativeFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return NativeFile{}, err
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return NativeFile{}, copyErr
	}
	if closeErr != nil {
		return NativeFile{}, closeErr
	}
	return NativeFile{Path: path, Bytes: bytesRead, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}
