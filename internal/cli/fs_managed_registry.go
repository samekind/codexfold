package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/samekind/codexfold/internal/storage"
)

const (
	managedSessionRegistryVersion       = 1
	managedSessionRegistryMaxBytes      = 1 << 20
	managedSessionRegistryFilename      = "managed-registry.json"
	managedSessionRegistryOperationLock = "managed-session-registry"
)

var (
	ErrManagedSessionRegistryCorrupt = errors.New("managed session registry is corrupt")
	ErrManagedSessionRegistryChanged = errors.New("managed session registry changed")
)

type ManagedSessionRegistryEntry struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
}

type ManagedSessionRegistry struct {
	Version  int                           `json:"version"`
	Revision uint64                        `json:"revision"`
	Entries  []ManagedSessionRegistryEntry `json:"entries"`
}

type ManagedSessionRegistryWriteOptions struct {
	// Bootstrap permits the first registry publication. Observed entries are
	// committed atomically with creation, never through an empty intermediate
	// registry.
	Bootstrap bool

	// ExpectedRevision turns the update into a compare-and-swap. Reload paths
	// must use it so an observation prepared before an exact retirement cannot
	// resurrect an entry removed by the retirement transaction.
	ExpectedRevision *uint64

	// RemovedSessionIDs must contain only removals already authorized by an
	// exact durable tombstone or completed retirement transaction. Every prior
	// entry not named here is retained when it is absent from the observation.
	RemovedSessionIDs map[string]struct{}

	// RemovalFence publishes a new revision even when every authorized removal
	// is already absent. Transaction completion uses it to fence stale writers
	// and to retry the directory fsync before clearing its durable request.
	RemovalFence bool
}

var managedSessionRegistrySyncHook func() error

func ManagedSessionRegistryPath(store string) string {
	return filepath.Join(filepath.Clean(store), "fs", managedSessionRegistryFilename)
}

func LoadManagedSessionRegistry(store string) (ManagedSessionRegistry, error) {
	storeRoot, err := openManagedSessionRegistryStore(store)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	defer storeRoot.Close()
	fsRoot, err := openManagedSessionRegistryDirectory(storeRoot, false)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	defer fsRoot.Close()
	return loadManagedSessionRegistryFromRoot(fsRoot)
}

func WriteManagedSessionRegistry(
	store string,
	observed []ManagedSessionRegistryEntry,
	options ManagedSessionRegistryWriteOptions,
) (ManagedSessionRegistry, error) {
	observedRegistry, err := normalizeManagedSessionRegistryEntries(observed)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	if err := validateManagedSessionRegistryRemovals(options.RemovedSessionIDs, observedRegistry.Entries); err != nil {
		return ManagedSessionRegistry{}, err
	}
	if options.RemovalFence && len(options.RemovedSessionIDs) == 0 {
		return ManagedSessionRegistry{}, errors.New("managed session registry removal fence requires an exact removal")
	}

	storeRoot, err := openManagedSessionRegistryStore(store)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	fsRoot, directoryErr := openManagedSessionRegistryDirectory(storeRoot, false)
	var existing ManagedSessionRegistry
	exists := false
	switch {
	case directoryErr == nil:
		existing, err = loadManagedSessionRegistryFromRoot(fsRoot)
		if err == nil {
			exists = true
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = fsRoot.Close()
			_ = storeRoot.Close()
			return ManagedSessionRegistry{}, err
		}
		_ = fsRoot.Close()
	case errors.Is(directoryErr, os.ErrNotExist):
	default:
		_ = storeRoot.Close()
		return ManagedSessionRegistry{}, directoryErr
	}
	_ = storeRoot.Close()

	if !exists {
		if !options.Bootstrap {
			return ManagedSessionRegistry{}, fmt.Errorf("managed session registry is missing outside bootstrap: %w", os.ErrNotExist)
		}
		if len(options.RemovedSessionIDs) != 0 {
			return ManagedSessionRegistry{}, errors.New("bootstrap managed session registry cannot apply removals")
		}
	}

	operation, err := storage.AcquireOperationLock(filepath.Clean(store), managedSessionRegistryOperationLock)
	if err != nil {
		return ManagedSessionRegistry{}, fmt.Errorf("serialize managed session registry update: %w", err)
	}
	defer operation.Close()

	storeRoot, err = openManagedSessionRegistryStore(store)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	defer storeRoot.Close()
	allowCreate := !exists && options.Bootstrap
	fsRoot, err = openManagedSessionRegistryDirectory(storeRoot, allowCreate)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	defer fsRoot.Close()

	missingUnderLock := false
	existing, err = loadManagedSessionRegistryFromRoot(fsRoot)
	if errors.Is(err, os.ErrNotExist) {
		if !allowCreate {
			return ManagedSessionRegistry{}, fmt.Errorf("managed session registry disappeared before update: %w", os.ErrNotExist)
		}
		missingUnderLock = true
		existing = ManagedSessionRegistry{Version: managedSessionRegistryVersion, Revision: 1, Entries: []ManagedSessionRegistryEntry{}}
	} else if err != nil {
		return ManagedSessionRegistry{}, err
	}
	if missingUnderLock && options.ExpectedRevision != nil {
		return ManagedSessionRegistry{}, fmt.Errorf("%w: registry disappeared before compare-and-swap", ErrManagedSessionRegistryChanged)
	}
	if options.ExpectedRevision != nil && existing.Revision != *options.ExpectedRevision {
		return ManagedSessionRegistry{}, fmt.Errorf("%w: expected revision %d, found %d", ErrManagedSessionRegistryChanged, *options.ExpectedRevision, existing.Revision)
	}

	merged, err := mergeManagedSessionRegistry(existing, observedRegistry, options.RemovedSessionIDs)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	if !missingUnderLock && equalManagedSessionRegistryEntries(existing, merged) && !options.RemovalFence {
		return existing, nil
	}
	if !missingUnderLock {
		if existing.Revision == ^uint64(0) {
			return ManagedSessionRegistry{}, errors.New("managed session registry revision overflow")
		}
		merged.Revision = existing.Revision + 1
	} else {
		merged.Revision = 1
	}
	if err := publishManagedSessionRegistry(fsRoot, merged); err != nil {
		return ManagedSessionRegistry{}, err
	}
	published, err := loadManagedSessionRegistryFromRoot(fsRoot)
	if err != nil {
		return ManagedSessionRegistry{}, fmt.Errorf("verify published managed session registry: %w", err)
	}
	if !equalManagedSessionRegistries(published, merged) {
		return ManagedSessionRegistry{}, errors.New("published managed session registry differs from the requested update")
	}
	return published, nil
}

func normalizeManagedSessionRegistryEntries(entries []ManagedSessionRegistryEntry) (ManagedSessionRegistry, error) {
	normalized := append([]ManagedSessionRegistryEntry(nil), entries...)
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	registry := ManagedSessionRegistry{Version: managedSessionRegistryVersion, Revision: 1, Entries: normalized}
	if registry.Entries == nil {
		registry.Entries = []ManagedSessionRegistryEntry{}
	}
	if err := validateManagedSessionRegistry(registry); err != nil {
		return ManagedSessionRegistry{}, err
	}
	return registry, nil
}

func validateManagedSessionRegistryRemovals(removals map[string]struct{}, observed []ManagedSessionRegistryEntry) error {
	if len(removals) == 0 {
		return nil
	}
	observedIDs := make(map[string]struct{}, len(observed))
	for _, entry := range observed {
		observedIDs[entry.ID] = struct{}{}
	}
	for sessionID := range removals {
		if !validSessionID(sessionID) {
			return errors.New("managed session registry removal contains an unsafe session ID")
		}
		if _, exists := observedIDs[sessionID]; exists {
			return fmt.Errorf("managed session %s cannot be observed and removed in the same registry update", sessionID)
		}
	}
	return nil
}

func mergeManagedSessionRegistry(existing ManagedSessionRegistry, observed ManagedSessionRegistry, removals map[string]struct{}) (ManagedSessionRegistry, error) {
	if err := validateManagedSessionRegistry(existing); err != nil {
		return ManagedSessionRegistry{}, err
	}
	if err := validateManagedSessionRegistry(observed); err != nil {
		return ManagedSessionRegistry{}, err
	}
	merged := make(map[string]uint64, len(existing.Entries)+len(observed.Entries))
	for _, entry := range existing.Entries {
		if _, removed := removals[entry.ID]; removed {
			continue
		}
		merged[entry.ID] = entry.Generation
	}
	for _, entry := range observed.Entries {
		if previous, exists := merged[entry.ID]; exists && entry.Generation < previous {
			return ManagedSessionRegistry{}, fmt.Errorf("managed session registry generation moved backwards for %s", entry.ID)
		}
		merged[entry.ID] = entry.Generation
	}
	entries := make([]ManagedSessionRegistryEntry, 0, len(merged))
	for sessionID, generation := range merged {
		entries = append(entries, ManagedSessionRegistryEntry{ID: sessionID, Generation: generation})
	}
	return normalizeManagedSessionRegistryEntries(entries)
}

func openManagedSessionRegistryStore(store string) (*os.Root, error) {
	store = filepath.Clean(store)
	if !filepath.IsAbs(store) {
		return nil, errors.New("absolute managed store path is required")
	}
	before, err := os.Lstat(store)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("managed session registry store is not a real directory")
	}
	root, err := os.OpenRoot(store)
	if err != nil {
		return nil, fmt.Errorf("open managed session registry store: %w", err)
	}
	opened, err := statManagedSessionRegistryRoot(root)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	after, err := os.Lstat(store)
	if err != nil || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = root.Close()
		if err == nil {
			err = errors.New("managed session registry store changed while it was opened")
		}
		return nil, err
	}
	return root, nil
}

func statManagedSessionRegistryRoot(root *os.Root) (os.FileInfo, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("managed session registry root is not a real directory")
	}
	return info, nil
}

func openManagedSessionRegistryDirectory(storeRoot *os.Root, create bool) (*os.Root, error) {
	before, err := storeRoot.Lstat("fs")
	if errors.Is(err, os.ErrNotExist) && create {
		if err := storeRoot.Mkdir("fs", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create managed session registry directory: %w", err)
		}
		if err := syncManagedSessionRegistryRoot(storeRoot); err != nil {
			return nil, err
		}
		before, err = storeRoot.Lstat("fs")
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &os.PathError{Op: "open", Path: "fs/" + managedSessionRegistryFilename, Err: os.ErrNotExist}
		}
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: managed session registry directory is not a real directory", ErrManagedSessionRegistryCorrupt)
	}
	fsRoot, err := storeRoot.OpenRoot("fs")
	if err != nil {
		return nil, fmt.Errorf("open managed session registry directory: %w", err)
	}
	opened, err := statManagedSessionRegistryRoot(fsRoot)
	if err != nil {
		_ = fsRoot.Close()
		return nil, err
	}
	after, err := storeRoot.Lstat("fs")
	if err != nil || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = fsRoot.Close()
		if err == nil {
			err = fmt.Errorf("%w: managed session registry directory changed while it was opened", ErrManagedSessionRegistryCorrupt)
		}
		return nil, err
	}
	return fsRoot, nil
}

func loadManagedSessionRegistryFromRoot(fsRoot *os.Root) (ManagedSessionRegistry, error) {
	before, err := fsRoot.Lstat(managedSessionRegistryFilename)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	if !before.Mode().IsRegular() {
		return ManagedSessionRegistry{}, fmt.Errorf("%w: registry is not an ordinary file", ErrManagedSessionRegistryCorrupt)
	}
	if before.Size() > managedSessionRegistryMaxBytes {
		return ManagedSessionRegistry{}, fmt.Errorf("%w: registry exceeds %d bytes", ErrManagedSessionRegistryCorrupt, managedSessionRegistryMaxBytes)
	}
	file, err := fsRoot.Open(managedSessionRegistryFilename)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return ManagedSessionRegistry{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || before.Size() != opened.Size() || !before.ModTime().Equal(opened.ModTime()) {
		_ = file.Close()
		return ManagedSessionRegistry{}, fmt.Errorf("%w: registry changed while it was opened", ErrManagedSessionRegistryCorrupt)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, managedSessionRegistryMaxBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return ManagedSessionRegistry{}, err
	}
	if len(data) > managedSessionRegistryMaxBytes {
		return ManagedSessionRegistry{}, fmt.Errorf("%w: registry exceeds %d bytes", ErrManagedSessionRegistryCorrupt, managedSessionRegistryMaxBytes)
	}
	after, err := fsRoot.Lstat(managedSessionRegistryFilename)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != int64(len(data)) || !opened.ModTime().Equal(after.ModTime()) {
		if err == nil {
			err = fmt.Errorf("%w: registry changed while it was read", ErrManagedSessionRegistryCorrupt)
		}
		return ManagedSessionRegistry{}, err
	}
	registry, err := decodeManagedSessionRegistry(data)
	if err != nil {
		return ManagedSessionRegistry{}, err
	}
	return registry, nil
}

func decodeManagedSessionRegistry(data []byte) (ManagedSessionRegistry, error) {
	var registry ManagedSessionRegistry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil {
		return ManagedSessionRegistry{}, fmt.Errorf("%w: decode registry: %v", ErrManagedSessionRegistryCorrupt, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return ManagedSessionRegistry{}, fmt.Errorf("%w: decode registry: %v", ErrManagedSessionRegistryCorrupt, err)
	}
	if err := validateManagedSessionRegistry(registry); err != nil {
		return ManagedSessionRegistry{}, err
	}
	return registry, nil
}

func validateManagedSessionRegistry(registry ManagedSessionRegistry) error {
	if registry.Version != managedSessionRegistryVersion || registry.Revision == 0 || registry.Entries == nil {
		return fmt.Errorf("%w: invalid registry version or entries", ErrManagedSessionRegistryCorrupt)
	}
	previous := ""
	for _, entry := range registry.Entries {
		if !validSessionID(entry.ID) || entry.Generation == 0 {
			return fmt.Errorf("%w: invalid registry entry", ErrManagedSessionRegistryCorrupt)
		}
		if previous != "" && entry.ID <= previous {
			return fmt.Errorf("%w: registry entries are not strictly sorted", ErrManagedSessionRegistryCorrupt)
		}
		previous = entry.ID
	}
	return nil
}

func publishManagedSessionRegistry(fsRoot *os.Root, registry ManagedSessionRegistry) error {
	if err := validateManagedSessionRegistry(registry); err != nil {
		return err
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > managedSessionRegistryMaxBytes {
		return fmt.Errorf("managed session registry exceeds %d bytes", managedSessionRegistryMaxBytes)
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return fmt.Errorf("create managed session registry publication token: %w", err)
	}
	temporaryName := ".managed-registry-" + hex.EncodeToString(token[:]) + ".tmp"
	temporary, err := fsRoot.OpenFile(temporaryName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary managed session registry: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = fsRoot.Remove(temporaryName)
		}
	}()
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary managed session registry: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary managed session registry: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary managed session registry: %w", err)
	}
	if current, err := fsRoot.Lstat(managedSessionRegistryFilename); err == nil {
		if !current.Mode().IsRegular() {
			return fmt.Errorf("%w: refusing to replace a non-ordinary registry", ErrManagedSessionRegistryCorrupt)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fsRoot.Rename(temporaryName, managedSessionRegistryFilename); err != nil {
		return fmt.Errorf("publish managed session registry: %w", err)
	}
	removeTemporary = false
	return syncManagedSessionRegistryRoot(fsRoot)
}

func syncManagedSessionRegistryRoot(root *os.Root) error {
	if managedSessionRegistrySyncHook != nil {
		if err := managedSessionRegistrySyncHook(); err != nil {
			return fmt.Errorf("sync managed session registry directory: %w", err)
		}
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync managed session registry directory: %w", err)
	}
	return nil
}

func equalManagedSessionRegistries(left ManagedSessionRegistry, right ManagedSessionRegistry) bool {
	if left.Version != right.Version || left.Revision != right.Revision || len(left.Entries) != len(right.Entries) {
		return false
	}
	for index := range left.Entries {
		if left.Entries[index] != right.Entries[index] {
			return false
		}
	}
	return true
}

func equalManagedSessionRegistryEntries(left ManagedSessionRegistry, right ManagedSessionRegistry) bool {
	if left.Version != right.Version || len(left.Entries) != len(right.Entries) {
		return false
	}
	for index := range left.Entries {
		if left.Entries[index] != right.Entries[index] {
			return false
		}
	}
	return true
}
