package storage

import (
	"context"
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
}

type StorageGCResult struct {
	StoreDir                  string        `json:"store_dir"`
	DryRun                    bool          `json:"dry_run"`
	Before                    Inventory     `json:"before"`
	After                     Inventory     `json:"after"`
	Candidates                []GCCandidate `json:"candidates,omitempty"`
	CandidateCount            int           `json:"candidate_count"`
	CandidateApparentBytes    int64         `json:"candidate_apparent_bytes"`
	ProjectedReclaimableBytes int64         `json:"projected_reclaimable_bytes"`
	RemovedCount              int           `json:"removed_count"`
	RemovedApparentBytes      int64         `json:"removed_apparent_bytes"`
	ActualReclaimedBytes      int64         `json:"actual_reclaimed_bytes"`
}

type gcBuilder struct {
	ctx        context.Context
	options    GCOptions
	scanner    *scanner
	candidates map[string]CandidateKind
}

type generationEntry struct {
	path      string
	name      string
	modTime   time.Time
	sequence  uint64
	sequenced bool
}

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
		allowed, err := builder.revalidate(candidate)
		if err != nil {
			return result, err
		}
		if !allowed {
			continue
		}
		if _, err := os.Lstat(candidate.Path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return result, err
		}
		if err := os.RemoveAll(candidate.Path); err != nil {
			return result, fmt.Errorf("remove storage GC candidate %s: %w", candidate.Path, err)
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
	writerActive, err := FileHasActiveLock(filepath.Join(directory, "writer.lease"))
	if err != nil {
		return false, err
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
		writerActive, err := FileHasActiveLock(filepath.Join(directory, "writer.lease"))
		if err != nil {
			return err
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
	switch candidate.Kind {
	case CandidatePackGeneration:
		data, err := os.ReadFile(filepath.Join(b.scanner.store, "packs", "CURRENT"))
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
		writerActive, err := FileHasActiveLock(filepath.Join(directory, "writer.lease"))
		if err != nil || writerActive {
			return false, err
		}
		readerActive, err := treeHasActiveLease(filepath.Join(directory, "leases"))
		return !readerActive, err
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
		for _, state := range b.scanner.managedStates {
			if cleanAbsolutePath(state.ManifestPath) == candidate.Path {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, errors.New("unknown storage GC candidate kind")
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
