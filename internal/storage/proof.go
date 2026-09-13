package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// ManagedSessionReference is the persisted relationship that must remain
// provable before storage shared by a managed session can be reclaimed.
type ManagedSessionReference struct {
	StoreDir       string
	SessionID      string
	ManifestPath   string
	ManifestSHA256 string
	BaseBytes      int64
	BaseSHA256     string
}

// ManagedSessionDeletionGuard keeps every managed session's writer lock held
// while shared storage is being reclaimed.
type ManagedSessionDeletionGuard struct {
	References []ManagedSessionReference
	files      []*os.File
}

// ErrManagedSessionBusy is ordinary live I/O contention, not corrupt storage.
var ErrManagedSessionBusy = errors.New("managed session is busy")

func ManagedSessionReferences(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
	s, exists, err := prepareScanner(ctx, Options{StoreDir: storeDir})
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return managedSessionReferences(s), nil
}

func (s *scanner) validateManagedReferences() error {
	for sessionID, state := range s.managedStates {
		if state.Version != 2 || !validSHA256(state.ManifestSHA256) {
			return fmt.Errorf("managed session %s has no exact manifest identity", sessionID)
		}
		manifestPath, err := cleanPathWithin(filepath.Join(s.store, "manifests"), state.ManifestPath)
		if err != nil {
			return fmt.Errorf("managed session %s manifest escapes the canonical manifest store: %w", sessionID, err)
		}
		reference := ManagedSessionReference{
			StoreDir: s.store, SessionID: sessionID, ManifestPath: manifestPath, ManifestSHA256: state.ManifestSHA256,
			BaseBytes: state.BaseBytes, BaseSHA256: state.BaseSHA256,
		}
		if err := ValidateManagedSessionReference(reference); err != nil {
			return fmt.Errorf("load current manifest for managed session %s: %w", sessionID, err)
		}
		for label, path := range map[string]string{"delta": state.DeltaPath, "backing": state.BackingPath} {
			if path == "" {
				continue
			}
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("stat managed session %s %s: %w", sessionID, label, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("managed session %s %s is not a regular file", sessionID, label)
			}
		}
	}
	return nil
}

// ValidateManagedSessionReference verifies the exact on-disk manifest bytes,
// not merely the decoded session and source fields. Destructive callers must
// reject legacy state that cannot name one exact manifest version.
func ValidateManagedSessionReference(reference ManagedSessionReference) error {
	if reference.SessionID == "" || !validSHA256(reference.ManifestSHA256) || reference.BaseBytes < 0 || !validSHA256(reference.BaseSHA256) {
		return errors.New("managed session reference is incomplete")
	}
	if reference.StoreDir == "" {
		return errors.New("managed session reference has no store root")
	}
	manifest, digest, err := loadManagedManifestIdentity(reference.StoreDir, reference.ManifestPath)
	if err != nil {
		return err
	}
	if digest != reference.ManifestSHA256 {
		return errors.New("manifest bytes do not match managed session state")
	}
	if manifest.Session.ID != reference.SessionID || manifest.Source.Bytes != reference.BaseBytes || manifest.Source.SHA256 != reference.BaseSHA256 {
		return errors.New("managed session state does not match its current manifest")
	}
	return nil
}

func loadManagedManifestIdentity(storeDir string, path string) (manifestRecord, string, error) {
	manifestRoot := filepath.Join(filepath.Clean(storeDir), "manifests")
	path, err := cleanPathWithin(manifestRoot, path)
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("resolve manifest %s: %w", path, err)
	}
	relative, err := filepath.Rel(manifestRoot, path)
	if err != nil {
		return manifestRecord{}, "", err
	}
	storeRoot, err := os.OpenRoot(filepath.Clean(storeDir))
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("open managed store root: %w", err)
	}
	defer storeRoot.Close()
	root, err := storeRoot.OpenRoot("manifests")
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("open manifest root: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat(relative)
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("inspect manifest %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return manifestRecord{}, "", fmt.Errorf("manifest %s is not a regular file", path)
	}
	file, err := root.Open(relative)
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("open manifest %s: %w", path, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("stat opened manifest %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return manifestRecord{}, "", fmt.Errorf("manifest %s changed while it was opened", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("read manifest %s: %w", path, err)
	}
	var manifest manifestRecord
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifestRecord{}, "", fmt.Errorf("decode manifest %s: %w", path, err)
	}
	if err := validateManifestRecord(manifest); err != nil {
		return manifestRecord{}, "", fmt.Errorf("invalid manifest %s: %w", path, err)
	}
	digest := sha256.Sum256(data)
	return manifest, hex.EncodeToString(digest[:]), nil
}

// ManagedSessionDeletionProof reloads every managed state and current manifest
// and refuses a destructive operation while a session can still change them.
func ManagedSessionDeletionProof(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
	guard, err := AcquireManagedSessionDeletionGuard(ctx, storeDir)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	return guard.References, nil
}

// AcquireManagedSessionDeletionGuard reloads every managed state and current
// manifest, then holds writer locks so that proof stays valid until Close.
func AcquireManagedSessionDeletionGuard(ctx context.Context, storeDir string) (*ManagedSessionDeletionGuard, error) {
	s, exists, err := prepareScanner(ctx, Options{StoreDir: storeDir})
	if err != nil {
		return nil, err
	}
	if !exists {
		return &ManagedSessionDeletionGuard{}, nil
	}
	references := managedSessionReferences(s)
	guard := &ManagedSessionDeletionGuard{References: references}
	for _, reference := range references {
		sessionID := reference.SessionID
		if err := ctx.Err(); err != nil {
			_ = guard.Close()
			return nil, err
		}
		directory := filepath.Join(s.store, "fs", "sessions", sessionID)
		writer, err := acquireExclusiveFileLock(filepath.Join(directory, "writer.lease"))
		if err != nil {
			_ = guard.Close()
			return nil, fmt.Errorf("lock managed session %s writer: %w", sessionID, err)
		}
		guard.files = append(guard.files, writer)
		readerActive, err := treeHasActiveLease(filepath.Join(directory, "leases"))
		if err != nil {
			_ = guard.Close()
			return nil, fmt.Errorf("inspect managed session %s readers: %w", sessionID, err)
		}
		pending, err := journalPendingAt(directory)
		if err != nil {
			_ = guard.Close()
			return nil, fmt.Errorf("inspect managed session %s journal: %w", sessionID, err)
		}
		if readerActive {
			_ = guard.Close()
			return nil, fmt.Errorf("%w: managed session %s has an active reader", ErrManagedSessionBusy, sessionID)
		}
		if pending {
			_ = guard.Close()
			return nil, fmt.Errorf("managed session %s has unfinished recovery", sessionID)
		}
	}
	if _, err := guard.Refresh(ctx, storeDir); err != nil {
		_ = guard.Close()
		return nil, fmt.Errorf("refresh managed sessions after locking writers: %w", err)
	}
	return guard, nil
}

func (g *ManagedSessionDeletionGuard) Refresh(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
	current, err := ManagedSessionReferences(ctx, storeDir)
	if err != nil {
		return nil, err
	}
	if len(current) != len(g.References) {
		return nil, errors.New("managed session set changed during deletion proof")
	}
	for index := range current {
		if current[index] != g.References[index] {
			return nil, fmt.Errorf("managed session %s changed during deletion proof", g.References[index].SessionID)
		}
	}
	return current, nil
}

func (g *ManagedSessionDeletionGuard) Close() error {
	if g == nil {
		return nil
	}
	var result error
	for index := len(g.files) - 1; index >= 0; index-- {
		file := g.files[index]
		result = errors.Join(result, unlockLease(file), file.Close())
	}
	g.files = nil
	return result
}

func acquireExclusiveFileLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockLease(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !locked {
		_ = file.Close()
		return nil, fmt.Errorf("%w: writer is active", ErrManagedSessionBusy)
	}
	return file, nil
}

func managedSessionReferences(s *scanner) []ManagedSessionReference {
	references := make([]ManagedSessionReference, 0, len(s.managedStates))
	for sessionID, state := range s.managedStates {
		references = append(references, ManagedSessionReference{
			StoreDir: s.store, SessionID: sessionID, ManifestPath: cleanAbsolutePath(state.ManifestPath), ManifestSHA256: state.ManifestSHA256,
			BaseBytes: state.BaseBytes, BaseSHA256: state.BaseSHA256,
		})
	}
	sort.Slice(references, func(i, j int) bool { return references[i].SessionID < references[j].SessionID })
	return references
}
