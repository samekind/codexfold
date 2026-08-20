package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type CandidateKind string

const (
	CandidatePackGeneration     CandidateKind = "pack-generation"
	CandidateManifestGeneration CandidateKind = "manifest-generation"
	CandidateSessionGeneration  CandidateKind = "session-generation"
	CandidateRetiredState       CandidateKind = "retired-state"
	CandidateTemporary          CandidateKind = "unowned-temporary"
)

type GCCandidate struct {
	Kind             CandidateKind `json:"kind"`
	Path             string        `json:"path"`
	Files            int           `json:"files"`
	ApparentBytes    int64         `json:"apparent_bytes"`
	PhysicalBytes    int64         `json:"physical_bytes"`
	ReclaimableBytes int64         `json:"reclaimable_bytes"`
}

type GCOptions struct {
	StoreDir                string
	Apply                   bool
	TemporaryGrace          time.Duration
	KeepPackGenerations     int
	KeepManifestGenerations int
	KeepRetiredPerSession   int
	Now                     func() time.Time
	BeforeRemove            func(GCCandidate) error
	// BeforePackStage runs after the pack guard's final canonical-path
	// revalidation and immediately before the candidate is atomically moved to
	// deletion staging. It exists for deterministic race testing.
	BeforePackStage func(GCCandidate) error
	// AuthorizePackGenerationRemoval must return a guard that keeps every
	// surviving reconstruction source stable until the candidate is removed.
	// A nil authorizer disables destructive pack-generation cleanup.
	AuthorizePackGenerationRemoval func(context.Context, GCCandidate) (PackGenerationRemovalGuard, error)
	// AuthorizeExactRemoval must verify a durable operation record and hold
	// every lock needed to keep the returned exact byte proof stable until
	// Close. A nil authorizer, or a nil guard for one candidate, retains that
	// candidate for diagnosis.
	AuthorizeExactRemoval func(context.Context, GCCandidate) (ExactRemovalGuard, error)
}

type PackGenerationRemovalGuard interface {
	Revalidate(context.Context) error
	RevalidateStaged(context.Context, GCCandidate) error
	Close() error
}

// ExactRemovalProof binds a destructive operation to one exact candidate
// tree. Age, naming conventions, generation counts, and an unreferenced path
// are never deletion proof by themselves.
type ExactRemovalProof struct {
	OperationID   string        `json:"operation_id"`
	Kind          CandidateKind `json:"kind"`
	Path          string        `json:"path"`
	Files         int           `json:"files"`
	ApparentBytes int64         `json:"apparent_bytes"`
	TreeSHA256    string        `json:"tree_sha256"`
}

// ExactRemovalGuard represents a caller-verified durable deletion authority.
// Revalidate must check that authority while its locks are still held. The
// storage package independently re-hashes the candidate before removal.
type ExactRemovalGuard interface {
	Proof() ExactRemovalProof
	Revalidate(context.Context) error
	Close() error
}

// CaptureExactRemovalProof records the current byte identity of one reported
// candidate. Capturing is not authorization: callers must durably persist the
// result before cleanup and later return a guard that revalidates that durable
// operation while holding the producer's mutation locks.
func CaptureExactRemovalProof(ctx context.Context, candidate GCCandidate, operationID string) (ExactRemovalProof, error) {
	proof := ExactRemovalProof{
		OperationID:   operationID,
		Kind:          candidate.Kind,
		Path:          candidate.Path,
		Files:         candidate.Files,
		ApparentBytes: candidate.ApparentBytes,
	}
	files, apparentBytes, digest, err := exactCandidateTreeIdentity(ctx, candidate.Path)
	if err != nil {
		return ExactRemovalProof{}, err
	}
	proof.TreeSHA256 = digest
	if files != candidate.Files || apparentBytes != candidate.ApparentBytes {
		return ExactRemovalProof{}, errors.New("storage GC candidate changed before exact proof capture")
	}
	if err := validateExactRemovalProofMetadata(candidate, proof); err != nil {
		return ExactRemovalProof{}, err
	}
	return proof, nil
}

type StorageGCResult struct {
	StoreDir                      string        `json:"store_dir"`
	DryRun                        bool          `json:"dry_run"`
	Before                        Inventory     `json:"before"`
	After                         Inventory     `json:"after"`
	Candidates                    []GCCandidate `json:"candidates,omitempty"`
	CandidateCount                int           `json:"candidate_count"`
	CandidateApparentBytes        int64         `json:"candidate_apparent_bytes"`
	ProjectedReclaimableBytes     int64         `json:"projected_reclaimable_bytes"`
	RemovedCount                  int           `json:"removed_count"`
	RemovedApparentBytes          int64         `json:"removed_apparent_bytes"`
	ActualReclaimedBytes          int64         `json:"actual_reclaimed_bytes"`
	RetainedUnprovedCount         int           `json:"retained_unproved_count"`
	RetainedUnprovedApparentBytes int64         `json:"retained_unproved_apparent_bytes"`
}

type gcBuilder struct {
	ctx           context.Context
	options       GCOptions
	scanner       *scanner
	candidates    map[string]CandidateKind
	lockedSession string
}

type generationEntry struct {
	path      string
	name      string
	modTime   time.Time
	sequence  uint64
	sequenced bool
}

const packGCStagingDirectoryName = ".gc-staging"

func Collect(ctx context.Context, options GCOptions) (StorageGCResult, error) {
	if options.StoreDir == "" {
		return StorageGCResult{}, errors.New("storage GC store directory is required")
	}
	if options.TemporaryGrace < 0 || options.KeepPackGenerations < 0 || options.KeepManifestGenerations < 0 || options.KeepRetiredPerSession < 0 {
		return StorageGCResult{}, errors.New("storage GC retention values cannot be negative")
	}
	if options.TemporaryGrace == 0 {
		options.TemporaryGrace = time.Hour
	}
	if options.KeepPackGenerations == 0 {
		options.KeepPackGenerations = 2
	}
	if options.KeepManifestGenerations == 0 {
		options.KeepManifestGenerations = 2
	}
	if options.KeepRetiredPerSession == 0 {
		options.KeepRetiredPerSession = 1
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	store := cleanAbsolutePath(options.StoreDir)
	before, err := Scan(ctx, Options{StoreDir: store, AllowMetadataIssues: true})
	if err != nil {
		return StorageGCResult{}, err
	}
	result := StorageGCResult{StoreDir: store, DryRun: !options.Apply, Before: before, After: before}
	metadata, exists, err := prepareScanner(ctx, Options{StoreDir: store, AllowMetadataIssues: true})
	if err != nil {
		return StorageGCResult{}, err
	}
	if !exists {
		return result, nil
	}
	builder := &gcBuilder{ctx: ctx, options: options, scanner: metadata, candidates: make(map[string]CandidateKind)}
	if err := builder.discoverPackGenerations(); err != nil {
		return StorageGCResult{}, err
	}
	if err := builder.discoverManifestGenerations(); err != nil {
		return StorageGCResult{}, err
	}
	if err := builder.discoverSessionGenerations(); err != nil {
		return StorageGCResult{}, err
	}
	if err := builder.discoverRetiredState(); err != nil {
		return StorageGCResult{}, err
	}
	if err := builder.discoverTemporaryFiles(); err != nil {
		return StorageGCResult{}, err
	}
	candidates, projected, err := describeCandidates(builder.candidates)
	if err != nil {
		return StorageGCResult{}, err
	}
	result.Candidates = candidates
	result.CandidateCount = len(candidates)
	result.ProjectedReclaimableBytes = projected
	for _, candidate := range candidates {
		result.CandidateApparentBytes += candidate.ApparentBytes
	}
	if !options.Apply {
		return result, nil
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var unlock func() error
		var packGuard PackGenerationRemovalGuard
		var exactGuard ExactRemovalGuard
		var exactProof ExactRemovalProof
		if candidate.Kind == CandidatePackGeneration {
			if options.AuthorizePackGenerationRemoval == nil {
				result.RetainedUnprovedCount++
				result.RetainedUnprovedApparentBytes += candidate.ApparentBytes
				continue
			}
			packGuard, err = options.AuthorizePackGenerationRemoval(ctx, candidate)
			if packGuard != nil {
				unlock = packGuard.Close
			}
		} else {
			if options.AuthorizeExactRemoval == nil {
				result.RetainedUnprovedCount++
				result.RetainedUnprovedApparentBytes += candidate.ApparentBytes
				continue
			}
			unlock, err = builder.lockCandidate(candidate)
			if err == nil {
				exactGuard, err = options.AuthorizeExactRemoval(ctx, candidate)
			}
			if exactGuard == nil && err == nil {
				if closeErr := unlock(); closeErr != nil {
					return result, closeErr
				}
				result.RetainedUnprovedCount++
				result.RetainedUnprovedApparentBytes += candidate.ApparentBytes
				continue
			}
			if err == nil {
				candidateUnlock := unlock
				unlock = func() error {
					return errors.Join(exactGuard.Close(), candidateUnlock())
				}
			}
		}
		if err != nil {
			if unlock != nil {
				_ = unlock()
			}
			return result, err
		}
		if unlock == nil {
			return result, errors.New("storage GC authorizer returned no pack-generation guard")
		}
		if exactGuard != nil {
			exactProof = exactGuard.Proof()
			if err := validateExactRemovalProofMetadata(candidate, exactProof); err != nil {
				_ = unlock()
				return result, err
			}
		}
		if options.BeforeRemove != nil {
			if err := options.BeforeRemove(candidate); err != nil {
				_ = unlock()
				return result, err
			}
		}
		allowed, err := builder.revalidate(candidate)
		if err != nil {
			_ = unlock()
			return result, err
		}
		if !allowed {
			if err := unlock(); err != nil {
				return result, err
			}
			continue
		}
		if _, err := os.Lstat(candidate.Path); errors.Is(err, os.ErrNotExist) {
			if err := unlock(); err != nil {
				return result, err
			}
			continue
		} else if err != nil {
			_ = unlock()
			return result, err
		}
		removalPath := candidate.Path
		var stagedPack *GCCandidate
		if packGuard != nil {
			if err := packGuard.Revalidate(ctx); err != nil {
				_ = unlock()
				return result, fmt.Errorf("refresh pack-generation byte proof: %w", err)
			}
			if options.BeforePackStage != nil {
				if err := options.BeforePackStage(candidate); err != nil {
					_ = unlock()
					return result, err
				}
			}
			staged, err := stagePackGenerationCandidate(store, candidate)
			if err != nil {
				_ = unlock()
				return result, fmt.Errorf("stage pack generation before removal: %w", err)
			}
			stagedPack = &staged
			removalPath = staged.Path
			if err := packGuard.RevalidateStaged(ctx, staged); err != nil {
				restoreErr := restoreStagedPackGeneration(store, candidate, staged)
				_ = unlock()
				return result, errors.Join(fmt.Errorf("revalidate staged pack generation: %w", err), restoreErr)
			}
		}
		if exactGuard != nil {
			if err := exactGuard.Revalidate(ctx); err != nil {
				_ = unlock()
				return result, fmt.Errorf("refresh exact storage deletion proof: %w", err)
			}
			if refreshed := exactGuard.Proof(); refreshed != exactProof {
				_ = unlock()
				return result, errors.New("exact storage deletion proof changed during authorization")
			}
			if err := validateExactRemovalProof(ctx, candidate, exactProof); err != nil {
				_ = unlock()
				return result, err
			}
		}
		if err := os.RemoveAll(removalPath); err != nil {
			_ = unlock()
			return result, fmt.Errorf("remove storage GC candidate %s: %w", removalPath, err)
		}
		if stagedPack != nil {
			if err := cleanupPackGenerationStaging(store, *stagedPack); err != nil {
				_ = unlock()
				return result, err
			}
		}
		if err := unlock(); err != nil {
			return result, err
		}
		result.RemovedCount++
		result.RemovedApparentBytes += candidate.ApparentBytes
	}
	after, err := Scan(ctx, Options{StoreDir: store, AllowMetadataIssues: true})
	if err != nil {
		return result, err
	}
	result.After = after
	if before.TotalPhysicalBytes > after.TotalPhysicalBytes {
		result.ActualReclaimedBytes = before.TotalPhysicalBytes - after.TotalPhysicalBytes
	}
	return result, nil
}

func stagePackGenerationCandidate(store string, candidate GCCandidate) (GCCandidate, error) {
	store = cleanAbsolutePath(store)
	packsDir := filepath.Join(store, "packs")
	generation := filepath.Base(candidate.Path)
	if candidate.Kind != CandidatePackGeneration || filepath.Clean(candidate.Path) != filepath.Join(packsDir, generation) || generation == "." || generation == ".." || strings.HasPrefix(generation, ".") {
		return GCCandidate{}, errors.New("pack generation staging requires a canonical generation directory")
	}
	root, err := os.OpenRoot(store)
	if err != nil {
		return GCCandidate{}, err
	}
	defer root.Close()
	if err := requirePlainGCDirectory(root, "packs"); err != nil {
		return GCCandidate{}, err
	}
	stagingRoot := filepath.Join("packs", packGCStagingDirectoryName)
	if err := root.Mkdir(stagingRoot, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return GCCandidate{}, err
	}
	if err := requirePlainGCDirectory(root, stagingRoot); err != nil {
		return GCCandidate{}, err
	}
	for range 32 {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return GCCandidate{}, err
		}
		token := hex.EncodeToString(random)
		tokenRoot := filepath.Join(stagingRoot, token)
		if err := root.Mkdir(tokenRoot, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return GCCandidate{}, err
		}
		if err := requirePlainGCDirectory(root, tokenRoot); err != nil {
			return GCCandidate{}, err
		}
		source := filepath.Join("packs", generation)
		sourceInfo, err := root.Lstat(source)
		if err != nil {
			_ = root.Remove(tokenRoot)
			return GCCandidate{}, err
		}
		if !sourceInfo.IsDir() || sourceInfo.Mode()&os.ModeSymlink != 0 {
			_ = root.Remove(tokenRoot)
			return GCCandidate{}, errors.New("pack generation candidate is not a plain directory")
		}
		target := filepath.Join(tokenRoot, generation)
		if err := root.Rename(source, target); err != nil {
			_ = root.Remove(tokenRoot)
			return GCCandidate{}, err
		}
		if err := syncGCRootDirectory(root, "packs"); err != nil {
			return GCCandidate{}, rollbackPackGenerationStage(root, source, target, tokenRoot, err)
		}
		if err := syncGCRootDirectory(root, tokenRoot); err != nil {
			return GCCandidate{}, rollbackPackGenerationStage(root, source, target, tokenRoot, err)
		}
		staged := candidate
		staged.Path = filepath.Join(store, target)
		return staged, nil
	}
	return GCCandidate{}, errors.New("could not allocate pack generation deletion staging")
}

func rollbackPackGenerationStage(root *os.Root, source string, target string, tokenRoot string, cause error) error {
	if _, err := root.Lstat(source); errors.Is(err, os.ErrNotExist) {
		if restoreErr := root.Rename(target, source); restoreErr != nil {
			return errors.Join(cause, fmt.Errorf("restore pack generation after staging sync failure: %w", restoreErr))
		}
		_ = syncGCRootDirectory(root, "packs")
		_ = root.Remove(tokenRoot)
		return cause
	} else if err != nil {
		return errors.Join(cause, err)
	}
	return errors.Join(cause, errors.New("pack generation replacement appeared after staging rename; staged evidence was preserved"))
}

func restoreStagedPackGeneration(store string, original GCCandidate, staged GCCandidate) error {
	store = cleanAbsolutePath(store)
	root, err := os.OpenRoot(store)
	if err != nil {
		return err
	}
	defer root.Close()
	originalRelative, err := filepath.Rel(store, filepath.Clean(original.Path))
	if err != nil {
		return err
	}
	stagedRelative, err := filepath.Rel(store, filepath.Clean(staged.Path))
	if err != nil {
		return err
	}
	if _, err := root.Lstat(originalRelative); err == nil {
		return errors.New("pack generation replacement appeared while staged evidence was preserved")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.Rename(stagedRelative, originalRelative); err != nil {
		return fmt.Errorf("restore unproved staged pack generation: %w", err)
	}
	if err := syncGCRootDirectory(root, "packs"); err != nil {
		return err
	}
	if err := cleanupPackGenerationStaging(store, staged); err != nil {
		return err
	}
	return nil
}

func cleanupPackGenerationStaging(store string, staged GCCandidate) error {
	tokenRoot := filepath.Dir(filepath.Clean(staged.Path))
	stagingRoot := filepath.Dir(tokenRoot)
	if filepath.Base(stagingRoot) != packGCStagingDirectoryName || filepath.Dir(stagingRoot) != filepath.Join(cleanAbsolutePath(store), "packs") {
		return errors.New("pack generation staging cleanup path is unsafe")
	}
	if err := os.Remove(tokenRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(stagingRoot)
	root, err := os.OpenRoot(cleanAbsolutePath(store))
	if err != nil {
		return err
	}
	defer root.Close()
	return syncGCRootDirectory(root, "packs")
}

func requirePlainGCDirectory(root *os.Root, relative string) error {
	info, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("storage GC path is not a plain directory: %s", relative)
	}
	return nil
}

func syncGCRootDirectory(root *os.Root, relative string) error {
	directory, err := root.Open(relative)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (b *gcBuilder) add(path string, kind CandidateKind) error {
	path = cleanAbsolutePath(path)
	if !pathWithin(b.scanner.store, path) || path == b.scanner.store {
		return errors.New("storage GC candidate escapes the store")
	}
	for existing := range b.candidates {
		if pathWithin(existing, path) {
			return nil
		}
		if pathWithin(path, existing) {
			delete(b.candidates, existing)
		}
	}
	b.candidates[path] = kind
	return nil
}

func (b *gcBuilder) covered(path string) bool {
	for candidate := range b.candidates {
		if pathWithin(candidate, path) {
			return true
		}
	}
	return false
}

func (b *gcBuilder) discoverPackGenerations() error {
	root := filepath.Join(b.scanner.store, "packs")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if b.scanner.currentPack == "" {
		return nil
	}
	var generations []generationEntry
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		generations = append(generations, generationEntry{path: filepath.Join(root, entry.Name()), name: entry.Name(), modTime: info.ModTime()})
	}
	sortGenerationEntries(generations)
	previousToKeep := max(0, b.options.KeepPackGenerations-1)
	for _, generation := range generations {
		if generation.name == b.scanner.currentPack {
			continue
		}
		active, err := DirectoryHasActiveLease(filepath.Join(generation.path, "leases"), false)
		if err != nil {
			return err
		}
		if active {
			continue
		}
		if previousToKeep > 0 {
			previousToKeep--
			continue
		}
		if err := b.add(generation.path, CandidatePackGeneration); err != nil {
			return err
		}
	}
	return nil
}

func (b *gcBuilder) discoverManifestGenerations() error {
	root := filepath.Join(b.scanner.store, "manifests", "generations")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionID := entry.Name()
		directory := filepath.Join(root, sessionID)
		if _, ok := b.scanner.managedStates[sessionID]; ok {
			maintenance, err := b.sessionMaintenanceActive(sessionID)
			if err != nil {
				return err
			}
			if maintenance || b.scanner.journalPending[filepath.Join(b.scanner.store, "fs", "sessions", sessionID)] {
				continue
			}
		}
		files, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		var generations []generationEntry
		for _, file := range files {
			if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
				continue
			}
			info, err := file.Info()
			if err != nil {
				return err
			}
			sequence, sequenced := manifestGenerationSequence(file.Name())
			generations = append(generations, generationEntry{path: filepath.Join(directory, file.Name()), name: file.Name(), modTime: info.ModTime(), sequence: sequence, sequenced: sequenced})
		}
		if len(generations) <= 1 {
			continue
		}
		current := ""
		currentSequence := uint64(0)
		currentSequenced := false
		if state, ok := b.scanner.managedStates[sessionID]; ok {
			current = cleanAbsolutePath(state.ManifestPath)
			if pathWithin(directory, current) {
				currentSequence, currentSequenced = manifestGenerationSequence(filepath.Base(current))
			}
		} else if _, ok := b.scanner.primaryManifests[sessionID]; ok {
			current = filepath.Join(b.scanner.store, "manifests", sessionID+".json")
		} else {
			continue
		}
		sortGenerationEntries(generations)
		previousToKeep := max(0, b.options.KeepManifestGenerations-1)
		for _, generation := range generations {
			if generation.path == current {
				continue
			}
			if currentSequenced {
				if !generation.sequenced {
					continue
				}
				if generation.sequence > currentSequence {
					if err := b.add(generation.path, CandidateManifestGeneration); err != nil {
						return err
					}
					continue
				}
			}
			if previousToKeep > 0 {
				previousToKeep--
				continue
			}
			if err := b.add(generation.path, CandidateManifestGeneration); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *gcBuilder) sessionMaintenanceActive(sessionID string) (bool, error) {
	directory := filepath.Join(b.scanner.store, "fs", "sessions", sessionID)
	writerActive := false
	if b.lockedSession != sessionID {
		var err error
		writerActive, err = FileHasActiveLock(filepath.Join(directory, "writer.lease"))
		if err != nil {
			return false, err
		}
	}
	readerActive, err := treeHasActiveLease(filepath.Join(directory, "leases"))
	if err != nil {
		return false, err
	}
	return writerActive || readerActive, nil
}

func manifestGenerationSequence(name string) (uint64, bool) {
	if filepath.Ext(name) != ".json" {
		return 0, false
	}
	sequence, err := strconv.ParseUint(strings.TrimSuffix(name, ".json"), 10, 64)
	return sequence, err == nil
}

func (b *gcBuilder) discoverSessionGenerations() error {
	for sessionID, state := range b.scanner.managedStates {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		directory := filepath.Join(b.scanner.store, "fs", "sessions", sessionID)
		writerActive := false
		if b.lockedSession != sessionID {
			var err error
			writerActive, err = FileHasActiveLock(filepath.Join(directory, "writer.lease"))
			if err != nil {
				return err
			}
		}
		readerActive, err := treeHasActiveLease(filepath.Join(directory, "leases"))
		if err != nil {
			return err
		}
		if writerActive || readerActive || b.scanner.journalPending[filepath.Clean(directory)] {
			continue
		}
		current := map[string]struct{}{cleanAbsolutePath(state.DeltaPath): {}}
		if state.BackingPath != "" {
			current[cleanAbsolutePath(state.BackingPath)] = struct{}{}
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			if _, keep := current[path]; keep {
				continue
			}
			if _, owned := b.scanner.journalOwned[path]; owned {
				continue
			}
			if isSessionGenerationData(b.scanner.store, path) {
				if err := b.add(path, CandidateSessionGeneration); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *gcBuilder) discoverRetiredState() error {
	root := filepath.Join(b.scanner.store, "fs", "retired")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	groups := make(map[string][]generationEntry)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(filepath.Join(directory, "state.json"))
		if err != nil {
			continue
		}
		var state struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(data, &state) != nil || state.SessionID == "" {
			continue
		}
		active, err := treeHasActiveLease(filepath.Join(directory, "leases"))
		if err != nil {
			return err
		}
		pending, err := journalPendingAt(directory)
		if err != nil {
			return err
		}
		if active || pending {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		groups[state.SessionID] = append(groups[state.SessionID], generationEntry{path: directory, name: entry.Name(), modTime: info.ModTime()})
	}
	for _, states := range groups {
		sortGenerationEntries(states)
		for index := b.options.KeepRetiredPerSession; index < len(states); index++ {
			if err := b.add(states[index].path, CandidateRetiredState); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *gcBuilder) discoverTemporaryFiles() error {
	cutoff := b.options.Now().Add(-b.options.TemporaryGrace)
	retiredRoot := filepath.Join(b.scanner.store, "fs", "retired")
	return filepath.WalkDir(b.scanner.store, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if path == b.scanner.store {
			return nil
		}
		if b.covered(path) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if pathWithin(retiredRoot, path) {
			if entry.IsDir() && path != retiredRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if _, owned := b.scanner.journalOwned[cleanAbsolutePath(path)]; owned {
			return nil
		}
		if !isUnownedTemporary(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if err := b.add(path, CandidateTemporary); err != nil {
			return err
		}
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

func (b *gcBuilder) revalidate(candidate GCCandidate) (bool, error) {
	fresh, exists, err := prepareScanner(b.ctx, Options{StoreDir: b.scanner.store})
	if err != nil {
		return false, fmt.Errorf("refresh storage GC deletion proof: %w", err)
	}
	if !exists {
		return false, nil
	}
	proof := &gcBuilder{ctx: b.ctx, options: b.options, scanner: fresh, candidates: make(map[string]CandidateKind), lockedSession: b.lockedSession}
	if err := proof.discoverCandidateKind(candidate.Kind); err != nil {
		return false, err
	}
	if proof.candidates[candidate.Path] != candidate.Kind {
		return false, nil
	}
	switch candidate.Kind {
	case CandidatePackGeneration:
		data, err := os.ReadFile(filepath.Join(fresh.store, "packs", "CURRENT"))
		if err != nil {
			return false, err
		}
		if filepath.Base(candidate.Path) == strings.TrimSpace(string(data)) {
			return false, nil
		}
		active, err := DirectoryHasActiveLease(filepath.Join(candidate.Path, "leases"), false)
		return !active, err
	case CandidateSessionGeneration:
		directory := filepath.Dir(candidate.Path)
		stateData, err := os.ReadFile(filepath.Join(directory, "state.json"))
		if err != nil {
			return false, err
		}
		var state stateRecord
		if err := json.Unmarshal(stateData, &state); err != nil {
			return false, err
		}
		if cleanAbsolutePath(state.DeltaPath) == candidate.Path || (state.BackingPath != "" && cleanAbsolutePath(state.BackingPath) == candidate.Path) {
			return false, nil
		}
		if b.lockedSession != filepath.Base(directory) {
			writerActive, err := FileHasActiveLock(filepath.Join(directory, "writer.lease"))
			if err != nil || writerActive {
				return false, err
			}
		}
		readerActive, err := treeHasActiveLease(filepath.Join(directory, "leases"))
		if err != nil || readerActive {
			return false, err
		}
		pending, err := journalPendingAt(directory)
		return !pending, err
	case CandidateTemporary:
		info, err := os.Stat(candidate.Path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return !info.ModTime().After(b.options.Now().Add(-b.options.TemporaryGrace)), nil
	case CandidateRetiredState:
		active, err := treeHasActiveLease(filepath.Join(candidate.Path, "leases"))
		if err != nil || active {
			return false, err
		}
		pending, err := journalPendingAt(candidate.Path)
		return !pending, err
	case CandidateManifestGeneration:
		for _, state := range fresh.managedStates {
			if cleanAbsolutePath(state.ManifestPath) == candidate.Path {
				return false, nil
			}
		}
		sessionID := filepath.Base(filepath.Dir(candidate.Path))
		directory := filepath.Join(fresh.store, "fs", "sessions", sessionID)
		maintenance, err := proof.sessionMaintenanceActive(sessionID)
		if err != nil || maintenance {
			return false, err
		}
		pending, err := journalPendingAt(directory)
		if err != nil || pending {
			return false, err
		}
		return true, nil
	default:
		return false, errors.New("unknown storage GC candidate kind")
	}
}

func (b *gcBuilder) lockCandidate(candidate GCCandidate) (func() error, error) {
	switch candidate.Kind {
	case CandidateManifestGeneration, CandidateSessionGeneration:
		sessionID := filepath.Base(filepath.Dir(candidate.Path))
		file, err := acquireExclusiveFileLock(filepath.Join(b.scanner.store, "fs", "sessions", sessionID, "writer.lease"))
		if err != nil {
			return nil, fmt.Errorf("lock managed session %s cleanup: %w", sessionID, err)
		}
		b.lockedSession = sessionID
		return func() error {
			b.lockedSession = ""
			return errors.Join(unlockLease(file), file.Close())
		}, nil
	default:
		return func() error { return nil }, nil
	}
}

func (b *gcBuilder) discoverCandidateKind(kind CandidateKind) error {
	switch kind {
	case CandidatePackGeneration:
		return b.discoverPackGenerations()
	case CandidateManifestGeneration:
		return b.discoverManifestGenerations()
	case CandidateSessionGeneration:
		return b.discoverSessionGenerations()
	case CandidateRetiredState:
		return b.discoverRetiredState()
	case CandidateTemporary:
		return b.discoverTemporaryFiles()
	default:
		return errors.New("unknown storage GC candidate kind")
	}
}

func sortGenerationEntries(entries []generationEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].sequenced && entries[j].sequenced && entries[i].sequence != entries[j].sequence {
			return entries[i].sequence > entries[j].sequence
		}
		if entries[i].sequenced != entries[j].sequenced {
			return entries[i].sequenced
		}
		if entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].name > entries[j].name
		}
		return entries[i].modTime.After(entries[j].modTime)
	})
}

func treeHasActiveLease(root string) (bool, error) {
	active := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		hasLease, err := DirectoryHasActiveLease(path, false)
		if err != nil {
			return err
		}
		if hasLease {
			active = true
			return filepath.SkipAll
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return active, err
}

func journalPendingAt(directory string) (bool, error) {
	path := filepath.Join(directory, "journal.jsonl")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	latest := make(map[string]string)
	for {
		var record journalRecord
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return false, err
		}
		latest[record.OperationID] = record.Phase
	}
	for _, phase := range latest {
		if phase != "complete" && phase != "rolled-back" {
			return true, nil
		}
	}
	return false, nil
}

type candidatePhysical struct {
	bytes          int64
	links          uint64
	candidateLinks uint64
	candidates     map[int]struct{}
}

func describeCandidates(paths map[string]CandidateKind) ([]GCCandidate, int64, error) {
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	candidates := make([]GCCandidate, len(ordered))
	physical := make(map[string]*candidatePhysical)
	for index, path := range ordered {
		candidate := GCCandidate{Kind: paths[path], Path: path}
		localPhysical := make(map[string]struct{})
		err := filepath.Walk(path, func(filePath string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			identity, bytes, err := physicalFile(filePath, info)
			if err != nil {
				return err
			}
			candidate.Files++
			candidate.ApparentBytes += info.Size()
			if _, seen := localPhysical[identity]; !seen {
				candidate.PhysicalBytes += bytes
				localPhysical[identity] = struct{}{}
			}
			item := physical[identity]
			if item == nil {
				links, err := physicalLinkCount(filePath, info)
				if err != nil {
					return err
				}
				item = &candidatePhysical{bytes: bytes, links: links, candidates: make(map[int]struct{})}
				physical[identity] = item
			}
			item.candidateLinks++
			item.candidates[index] = struct{}{}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		candidates[index] = candidate
	}
	var projected int64
	for _, item := range physical {
		if item.candidateLinks < item.links {
			continue
		}
		projected += item.bytes
		first := len(candidates)
		for index := range item.candidates {
			if index < first {
				first = index
			}
		}
		if first < len(candidates) {
			candidates[first].ReclaimableBytes += item.bytes
		}
	}
	return candidates, projected, nil
}

func validateExactRemovalProofMetadata(candidate GCCandidate, proof ExactRemovalProof) error {
	if proof.OperationID == "" || proof.OperationID != strings.TrimSpace(proof.OperationID) || len(proof.OperationID) > 1024 || strings.ContainsRune(proof.OperationID, '\x00') {
		return errors.New("exact storage deletion proof has an invalid durable operation ID")
	}
	if proof.Kind != candidate.Kind {
		return errors.New("exact storage deletion proof has the wrong candidate kind")
	}
	if cleanAbsolutePath(proof.Path) != candidate.Path || proof.Path != cleanAbsolutePath(proof.Path) {
		return errors.New("exact storage deletion proof has the wrong candidate path")
	}
	if proof.Files < 0 || proof.ApparentBytes < 0 || !validSHA256(proof.TreeSHA256) {
		return errors.New("exact storage deletion proof has an invalid byte identity")
	}
	if proof.Files != candidate.Files || proof.ApparentBytes != candidate.ApparentBytes {
		return errors.New("exact storage deletion proof does not match the inventoried candidate bytes")
	}
	return nil
}

func validateExactRemovalProof(ctx context.Context, candidate GCCandidate, proof ExactRemovalProof) error {
	if err := validateExactRemovalProofMetadata(candidate, proof); err != nil {
		return err
	}
	files, apparentBytes, digest, err := exactCandidateTreeIdentity(ctx, candidate.Path)
	if err != nil {
		return fmt.Errorf("hash exact storage deletion candidate: %w", err)
	}
	if files != proof.Files || apparentBytes != proof.ApparentBytes || digest != proof.TreeSHA256 {
		return errors.New("storage GC candidate bytes differ from the exact durable deletion proof")
	}
	return nil
}

func exactCandidateTreeIdentity(ctx context.Context, root string) (int, int64, string, error) {
	root = cleanAbsolutePath(root)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return 0, 0, "", err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || (!rootInfo.IsDir() && !rootInfo.Mode().IsRegular()) {
		return 0, 0, "", errors.New("exact deletion candidate must be a regular file or plain directory")
	}
	treeHash := sha256.New()
	files := 0
	var apparentBytes int64
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("exact deletion candidate traversal escaped its root")
		}
		record := struct {
			Path   string `json:"path"`
			Kind   string `json:"kind"`
			Bytes  int64  `json:"bytes,omitempty"`
			SHA256 string `json:"sha256,omitempty"`
		}{Path: filepath.ToSlash(relative)}
		switch {
		case info.IsDir() && info.Mode()&os.ModeSymlink == 0:
			record.Kind = "directory"
		case info.Mode().IsRegular():
			record.Kind = "regular"
			record.Bytes = info.Size()
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			opened, err := file.Stat()
			if err != nil {
				_ = file.Close()
				return err
			}
			if !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() != info.Size() {
				_ = file.Close()
				return errors.New("exact deletion candidate changed while opening a file")
			}
			contentHash := sha256.New()
			bytesRead, copyErr := io.Copy(contentHash, file)
			after, statErr := file.Stat()
			closeErr := file.Close()
			if err := errors.Join(copyErr, statErr, closeErr); err != nil {
				return err
			}
			if bytesRead != info.Size() || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
				return errors.New("exact deletion candidate changed while hashing a file")
			}
			current, err := os.Lstat(path)
			if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) || current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) {
				if err != nil {
					return err
				}
				return errors.New("exact deletion candidate changed after hashing a file")
			}
			record.SHA256 = hex.EncodeToString(contentHash.Sum(nil))
			files++
			var overflow bool
			apparentBytes, overflow = addInt64(apparentBytes, info.Size())
			if overflow {
				return errors.New("exact deletion candidate byte count overflows")
			}
		default:
			return fmt.Errorf("exact deletion candidate contains an unsupported entry: %s", path)
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := treeHash.Write(append(encoded, '\n')); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, 0, "", err
	}
	return files, apparentBytes, hex.EncodeToString(treeHash.Sum(nil)), nil
}
