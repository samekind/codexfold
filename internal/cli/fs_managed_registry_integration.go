package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

type managedSessionRegistryFinalSnapshot struct {
	Registry  ManagedSessionRegistry
	Retained  map[string]uint64
	States    []vfs.SessionState
	Issues    []vfs.SessionStateIssue
	Deletions []vfs.SessionDeletion
}

// managedSessionRegistryBootstrapEntries permits first publication only when
// the current managed-session root is directly observable, or the store has no
// durable evidence that managed data ever existed. Once the registry exists,
// ordinary reloads never use this bootstrap path.
func managedSessionRegistryBootstrapEntries(store string, retained map[string]uint64) ([]ManagedSessionRegistryEntry, error) {
	entries := managedSessionRegistryEntriesFromGenerations(retained)
	if len(entries) != len(retained) {
		return nil, errors.New("managed session registry cannot bootstrap while a retained state generation is unavailable")
	}
	store = filepath.Clean(store)
	root, err := os.OpenRoot(store)
	if errors.Is(err, os.ErrNotExist) {
		if len(entries) == 0 {
			return entries, nil
		}
		return nil, errors.New("managed session registry cannot bootstrap retained sessions without the store root")
	}
	if err != nil {
		return nil, fmt.Errorf("open store before managed session registry bootstrap: %w", err)
	}
	defer root.Close()

	sessionsInfo, err := root.Lstat(filepath.Join("fs", "sessions"))
	if err == nil {
		if !sessionsInfo.IsDir() || sessionsInfo.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("managed session registry cannot bootstrap from an unsafe sessions root")
		}
		return entries, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect sessions root before managed session registry bootstrap: %w", err)
	}
	if len(entries) != 0 {
		return nil, errors.New("managed session registry cannot bootstrap retained sessions while the sessions root is missing")
	}

	for _, relative := range []string{
		"manifests",
		filepath.Join("fs", "retired"),
		filepath.Join("fs", "snapshots"),
		filepath.Join("fs", "deletions"),
	} {
		before, lstatErr := root.Lstat(relative)
		if errors.Is(lstatErr, os.ErrNotExist) {
			continue
		}
		if lstatErr != nil {
			return nil, fmt.Errorf("inspect managed evidence %s before registry bootstrap: %w", relative, lstatErr)
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("inspect managed evidence %s before registry bootstrap: managed evidence root is not a real directory", relative)
		}
		directory, openErr := root.Open(relative)
		if openErr != nil {
			return nil, fmt.Errorf("inspect managed evidence %s before registry bootstrap: %w", relative, openErr)
		}
		info, statErr := directory.Stat()
		after, afterErr := root.Lstat(relative)
		if statErr != nil || afterErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, info) || !os.SameFile(info, after) {
			_ = directory.Close()
			statErr = errors.Join(statErr, afterErr)
			if statErr == nil {
				statErr = errors.New("managed evidence root is not a real directory")
			}
			return nil, fmt.Errorf("inspect managed evidence %s before registry bootstrap: %w", relative, statErr)
		}
		contents, readErr := directory.ReadDir(1)
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		closeErr := directory.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, fmt.Errorf("read managed evidence %s before registry bootstrap: %w", relative, err)
		}
		if len(contents) != 0 {
			return nil, fmt.Errorf("managed session registry is missing while durable managed evidence remains in %s", relative)
		}
	}
	return entries, nil
}

func removeManagedSessionRegistryEntryIfPresent(store string, sessionID string) error {
	if !validSessionID(sessionID) {
		return errors.New("safe session ID is required for managed session registry removal")
	}
	for attempt := 0; attempt < 8; attempt++ {
		registry, err := LoadManagedSessionRegistry(store)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		revision := registry.Revision
		_, err = WriteManagedSessionRegistry(
			store,
			nil,
			ManagedSessionRegistryWriteOptions{
				ExpectedRevision:  &revision,
				RemovedSessionIDs: map[string]struct{}{sessionID: {}},
				RemovalFence:      true,
			},
		)
		if errors.Is(err, ErrManagedSessionRegistryChanged) {
			continue
		}
		return err
	}
	return ErrManagedSessionRegistryChanged
}

func upsertManagedSessionRegistryEntryIfPresent(store string, sessionID string, generation uint64) error {
	if !validSessionID(sessionID) || generation == 0 {
		return errors.New("safe session ID and generation are required for managed session registry publication")
	}
	for attempt := 0; attempt < 8; attempt++ {
		registry, err := LoadManagedSessionRegistry(store)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		revision := registry.Revision
		_, err = WriteManagedSessionRegistry(
			store,
			[]ManagedSessionRegistryEntry{{ID: sessionID, Generation: generation}},
			ManagedSessionRegistryWriteOptions{ExpectedRevision: &revision},
		)
		if errors.Is(err, ErrManagedSessionRegistryChanged) {
			continue
		}
		return err
	}
	return ErrManagedSessionRegistryChanged
}

func publishManagedSessionRegistryAtFinalFence(
	store string,
	known map[string]uint64,
	transactionRemovals map[string]struct{},
) (snapshot managedSessionRegistryFinalSnapshot, resultErr error) {
	retirementLock, err := storage.AcquireOperationLock(store, canonicalRetirementRecoveryLock)
	if err != nil {
		return snapshot, fmt.Errorf("serialize final registry publication with retirement recovery: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, retirementLock.Close()) }()
	deletionLock, err := storage.AcquireOperationLock(store, "session-deletions")
	if err != nil {
		return snapshot, fmt.Errorf("serialize final registry publication with explicit deletion: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, deletionLock.Close()) }()

	snapshot.Deletions, err = vfs.DiscoverSessionDeletions(store)
	if err != nil {
		return snapshot, err
	}
	snapshot.States, snapshot.Issues, err = vfs.DiscoverSessionStatesDetailedReadOnly(store)
	if err != nil {
		return snapshot, err
	}
	deleted := deletedSessionIDs(snapshot.Deletions)
	snapshot.States, snapshot.Issues = filterDeletedSessionMetadata(snapshot.States, snapshot.Issues, deleted)
	snapshot.Registry, err = LoadManagedSessionRegistry(store)
	if err != nil {
		return snapshot, err
	}

	retainedIDs := managedSessionIDsRetained(snapshot.States, snapshot.Issues)
	registryIDs := make(map[string]struct{}, len(snapshot.Registry.Entries))
	snapshot.Retained = retainedManagedSessionGenerations(nil, snapshot.States, snapshot.Issues)
	mergeManagedSessionRegistryGenerations(snapshot.Retained, snapshot.Registry)
	for _, entry := range snapshot.Registry.Entries {
		registryIDs[entry.ID] = struct{}{}
	}
	for sessionID, generation := range known {
		_, retainedByMetadata := retainedIDs[sessionID]
		_, retainedByRegistry := registryIDs[sessionID]
		if (retainedByMetadata || retainedByRegistry) && generation > snapshot.Retained[sessionID] {
			snapshot.Retained[sessionID] = generation
		}
	}
	removals := make(map[string]struct{}, len(deleted)+len(transactionRemovals))
	for sessionID := range deleted {
		removals[sessionID] = struct{}{}
	}
	for sessionID := range transactionRemovals {
		removals[sessionID] = struct{}{}
	}
	for sessionID := range removals {
		delete(snapshot.Retained, sessionID)
	}
	revision := snapshot.Registry.Revision
	snapshot.Registry, err = WriteManagedSessionRegistry(
		store,
		managedSessionRegistryEntriesFromGenerations(snapshot.Retained),
		ManagedSessionRegistryWriteOptions{ExpectedRevision: &revision, RemovedSessionIDs: removals},
	)
	if err != nil {
		return snapshot, err
	}
	snapshot.Retained = make(map[string]uint64, len(snapshot.Registry.Entries))
	mergeManagedSessionRegistryGenerations(snapshot.Retained, snapshot.Registry)
	for sessionID := range removals {
		delete(known, sessionID)
	}
	return snapshot, nil
}
