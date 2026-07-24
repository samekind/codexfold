package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type FileUsage struct {
	Files         int   `json:"files"`
	ApparentBytes int64 `json:"apparent_bytes"`
	PhysicalBytes int64 `json:"physical_bytes"`
}

type Inventory struct {
	StoreDir            string    `json:"store_dir"`
	LogicalSessionBytes int64     `json:"logical_session_bytes"`
	UniqueLooseObjects  FileUsage `json:"unique_loose_objects"`
	Packs               FileUsage `json:"packs"`
	NativeSources       FileUsage `json:"native_sources"`
	RetainedSnapshots   FileUsage `json:"retained_snapshots"`
	CurrentFallbacks    FileUsage `json:"current_fallbacks"`
	ActiveDeltas        FileUsage `json:"active_deltas"`
	WritableBackings    FileUsage `json:"writable_backings"`
	OldGenerations      FileUsage `json:"old_generations"`
	RetirementState     FileUsage `json:"retirement_state"`
	JournalRecovery     FileUsage `json:"journal_recovery"`
	UnownedTemporary    FileUsage `json:"unowned_temporary"`
	Metadata            FileUsage `json:"metadata"`
	TotalFiles          int       `json:"total_files"`
	UniquePhysicalFiles int       `json:"unique_physical_files"`
	HardlinkAliases     int       `json:"hardlink_aliases"`
	TotalApparentBytes  int64     `json:"total_apparent_bytes"`
	TotalPhysicalBytes  int64     `json:"total_physical_bytes"`
	IssueCount          int       `json:"issue_count"`
	Issues              []string  `json:"issues,omitempty"`
}

type Options struct {
	StoreDir            string
	AllowMetadataIssues bool
}

type manifestRecord struct {
	Session struct {
		ID          string `json:"id"`
		RolloutPath string `json:"rollout_path"`
	} `json:"session"`
	Source struct {
		Bytes int64 `json:"bytes"`
	} `json:"source"`
}

type stateRecord struct {
	SessionID    string `json:"session_id"`
	Generation   uint64 `json:"generation"`
	ManifestPath string `json:"manifest_path"`
	BaseBytes    int64  `json:"base_bytes"`
	DeltaPath    string `json:"delta_path"`
	BackingPath  string `json:"backing_path"`
	Native       struct {
		Path string `json:"path"`
	} `json:"native_snapshot"`
}

type journalRecord struct {
	OperationID string `json:"operation_id"`
	Phase       string `json:"phase"`
	TempPath    string `json:"temp_path"`
	FinalPath   string `json:"final_path"`
	Native      struct {
		Path string `json:"path"`
	} `json:"native"`
}

type scanner struct {
	ctx                 context.Context
	store               string
	canonicalStore      string
	nestedMounts        map[string]struct{}
	result              Inventory
	physicalFiles       map[string]struct{}
	primaryManifests    map[string]manifestRecord
	managedStates       map[string]stateRecord
	activeDeltas        map[string]struct{}
	backings            map[string]struct{}
	snapshots           map[string]struct{}
	journalOwned        map[string]struct{}
	journalPending      map[string]bool
	currentPack         string
	allowMetadataIssues bool
}

func Scan(ctx context.Context, options Options) (Inventory, error) {
	s, exists, err := prepareScanner(ctx, options)
	if err != nil {
		return Inventory{}, err
	}
	if !exists {
		return Inventory{StoreDir: cleanAbsolutePath(options.StoreDir)}, nil
	}
	if err := s.calculateLogicalBytes(); err != nil {
		return Inventory{}, err
	}
	if err := s.walkStore(); err != nil {
		return Inventory{}, err
	}
	if err := s.addExternalReferences(); err != nil {
		return Inventory{}, err
	}
	return s.result, nil
}

func prepareScanner(ctx context.Context, options Options) (*scanner, bool, error) {
	if options.StoreDir == "" {
		return nil, false, errors.New("storage inventory store directory is required")
	}
	store, err := filepath.Abs(options.StoreDir)
	if err != nil {
		return nil, false, fmt.Errorf("resolve storage inventory root: %w", err)
	}
	store = filepath.Clean(store)
	if info, err := os.Stat(store); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat storage inventory root: %w", err)
	} else if !info.IsDir() {
		return nil, false, errors.New("storage inventory root is not a directory")
	}
	canonicalStore := store
	if resolved, err := filepath.EvalSymlinks(store); err == nil {
		canonicalStore = filepath.Clean(resolved)
	}
	nestedMounts, err := nestedMountPoints(canonicalStore)
	if err != nil {
		return nil, false, fmt.Errorf("inspect nested storage mounts: %w", err)
	}
	s := &scanner{
		ctx: ctx, store: store, result: Inventory{StoreDir: store},
		canonicalStore: canonicalStore, nestedMounts: nestedMounts,
		physicalFiles: make(map[string]struct{}), primaryManifests: make(map[string]manifestRecord),
		managedStates: make(map[string]stateRecord), activeDeltas: make(map[string]struct{}),
		backings: make(map[string]struct{}), snapshots: make(map[string]struct{}),
		journalOwned: make(map[string]struct{}), journalPending: make(map[string]bool),
		allowMetadataIssues: options.AllowMetadataIssues,
	}
	if err := s.loadManifests(); err != nil {
		return nil, false, err
	}
	if err := s.loadStatesAndJournals(); err != nil {
		return nil, false, err
	}
	if err := s.loadCurrentPack(); err != nil {
		return nil, false, err
	}
	return s, true, nil
}

func (s *scanner) loadManifests() error {
	root := filepath.Join(s.store, "manifests")
	return walkIfPresent(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		manifest, err := decodeManifest(path)
		if err != nil {
			return s.metadataIssue(err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if filepath.Dir(relative) != "." {
			return nil
		}
		if manifest.Session.ID == "" || manifest.Source.Bytes < 0 {
			return s.metadataIssue(fmt.Errorf("invalid primary manifest %s", path))
		}
		if _, exists := s.primaryManifests[manifest.Session.ID]; exists {
			return s.metadataIssue(fmt.Errorf("duplicate primary manifest for session %s", manifest.Session.ID))
		}
		s.primaryManifests[manifest.Session.ID] = manifest
		return nil
	})
}

func (s *scanner) loadStatesAndJournals() error {
	root := filepath.Join(s.store, "fs", "sessions")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read managed session directories: %w", err)
	}
	for _, entry := range entries {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		statePath := filepath.Join(directory, "state.json")
		data, err := os.ReadFile(statePath)
		if err != nil {
			return fmt.Errorf("read managed session state %s: %w", entry.Name(), err)
		}
		var state stateRecord
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("decode managed session state %s: %w", entry.Name(), err)
		}
		if state.SessionID != entry.Name() || state.Generation == 0 || state.BaseBytes < 0 || state.DeltaPath == "" {
			return fmt.Errorf("invalid managed session state %s", statePath)
		}
		state.DeltaPath, err = cleanPathWithin(directory, state.DeltaPath)
		if err != nil {
			return fmt.Errorf("invalid managed delta for %s: %w", state.SessionID, err)
		}
		if state.BackingPath != "" {
			state.BackingPath, err = cleanPathWithin(directory, state.BackingPath)
			if err != nil {
				return fmt.Errorf("invalid writable backing for %s: %w", state.SessionID, err)
			}
			s.backings[state.BackingPath] = struct{}{}
		} else {
			s.activeDeltas[state.DeltaPath] = struct{}{}
		}
		if state.Native.Path != "" {
			state.Native.Path = cleanAbsolutePath(state.Native.Path)
			s.snapshots[state.Native.Path] = struct{}{}
		}
		s.managedStates[state.SessionID] = state
		if err := s.loadJournal(directory); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) loadJournal(directory string) error {
	path := filepath.Join(directory, "journal.jsonl")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open session journal: %w", err)
	}
	defer file.Close()
	latest := make(map[string]journalRecord)
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for lines.Scan() {
		var record journalRecord
		if err := json.Unmarshal(lines.Bytes(), &record); err != nil {
			return fmt.Errorf("decode session journal %s: %w", path, err)
		}
		if record.OperationID == "" {
			return fmt.Errorf("session journal %s contains a record without an operation ID", path)
		}
		latest[record.OperationID] = record
	}
	if err := lines.Err(); err != nil {
		return fmt.Errorf("read session journal %s: %w", path, err)
	}
	for _, record := range latest {
		if record.Phase == "complete" || record.Phase == "rolled-back" {
			continue
		}
		s.journalPending[filepath.Clean(directory)] = true
		for _, candidate := range []string{record.TempPath, record.FinalPath, record.Native.Path} {
			if candidate == "" {
				continue
			}
			clean, err := cleanPathWithin(directory, candidate)
			if err != nil {
				return fmt.Errorf("unsafe journal-owned recovery path: %w", err)
			}
			s.journalOwned[clean] = struct{}{}
		}
	}
	return nil
}

func (s *scanner) calculateLogicalBytes() error {
	for sessionID, state := range s.managedStates {
		var bytes int64
		if state.BackingPath != "" {
			info, err := os.Stat(state.BackingPath)
			if err != nil {
				return fmt.Errorf("stat writable backing for %s: %w", sessionID, err)
			}
			bytes = info.Size()
		} else {
			info, err := os.Stat(state.DeltaPath)
			if err != nil {
				return fmt.Errorf("stat active delta for %s: %w", sessionID, err)
			}
			var overflow bool
			bytes, overflow = addInt64(state.BaseBytes, info.Size())
			if overflow {
				return fmt.Errorf("logical bytes overflow for managed session %s", sessionID)
			}
		}
		var overflow bool
		s.result.LogicalSessionBytes, overflow = addInt64(s.result.LogicalSessionBytes, bytes)
		if overflow {
			return errors.New("logical session byte total overflow")
		}
	}
	for sessionID, manifest := range s.primaryManifests {
		if _, managed := s.managedStates[sessionID]; managed {
			continue
		}
		var overflow bool
		s.result.LogicalSessionBytes, overflow = addInt64(s.result.LogicalSessionBytes, manifest.Source.Bytes)
		if overflow {
			return errors.New("logical session byte total overflow")
		}
	}
	return nil
}

func (s *scanner) loadCurrentPack() error {
	data, err := os.ReadFile(filepath.Join(s.store, "packs", "CURRENT"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current pack generation: %w", err)
	}
	s.currentPack = strings.TrimSpace(string(data))
	if s.currentPack == "" || filepath.Base(s.currentPack) != s.currentPack || s.currentPack == "." || s.currentPack == ".." {
		s.currentPack = ""
		return s.metadataIssue(errors.New("invalid current pack generation"))
	}
	return nil
}

func (s *scanner) metadataIssue(err error) error {
	if !s.allowMetadataIssues {
		return err
	}
	s.result.Issues = append(s.result.Issues, err.Error())
	s.result.IssueCount = len(s.result.Issues)
	return nil
}

func (s *scanner) walkStore() error {
	return filepath.WalkDir(s.store, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if s.isNestedMount(path) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		usage := s.classifyStorePath(path)
		return s.addFile(usage, path, info)
	})
}

func (s *scanner) isNestedMount(path string) bool {
	if path == s.store || len(s.nestedMounts) == 0 {
		return false
	}
	relative, err := filepath.Rel(s.store, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	_, mounted := s.nestedMounts[filepath.Clean(filepath.Join(s.canonicalStore, relative))]
	return mounted
}

func (s *scanner) classifyStorePath(path string) *FileUsage {
	path = filepath.Clean(path)
	if _, ok := s.backings[path]; ok {
		return &s.result.WritableBackings
	}
	if _, ok := s.activeDeltas[path]; ok {
		return &s.result.ActiveDeltas
	}
	if _, ok := s.journalOwned[path]; ok {
		return &s.result.JournalRecovery
	}
	if pathWithin(filepath.Join(s.store, "fs", "retired"), path) {
		return &s.result.RetirementState
	}
	if pathWithin(filepath.Join(s.store, "fs", "snapshots"), path) {
		return &s.result.RetainedSnapshots
	}
	if pathWithin(filepath.Join(s.store, "fs", "fallbacks"), path) && isCurrentFallback(path) {
		return &s.result.CurrentFallbacks
	}
	if isUnownedTemporary(path) {
		return &s.result.UnownedTemporary
	}
	if pathWithin(filepath.Join(s.store, "objects"), path) && filepath.Ext(path) == ".zst" {
		return &s.result.UniqueLooseObjects
	}
	if generation, ok := packGeneration(s.store, path); ok {
		if generation == s.currentPack {
			return &s.result.Packs
		}
		return &s.result.OldGenerations
	}
	if pathWithin(filepath.Join(s.store, "manifests", "generations"), path) {
		return &s.result.OldGenerations
	}
	if isSessionGenerationData(s.store, path) {
		return &s.result.OldGenerations
	}
	return &s.result.Metadata
}

func (s *scanner) addExternalReferences() error {
	for path := range s.snapshots {
		if pathWithin(s.store, path) {
			continue
		}
		if err := s.addExternalFile(&s.result.RetainedSnapshots, path, true); err != nil {
			return err
		}
	}
	nativePaths := make(map[string]struct{})
	for sessionID, manifest := range s.primaryManifests {
		if _, managed := s.managedStates[sessionID]; managed {
			continue
		}
		if manifest.Session.RolloutPath != "" {
			nativePaths[cleanAbsolutePath(manifest.Session.RolloutPath)] = struct{}{}
		}
	}
	for path := range nativePaths {
		if pathWithin(s.store, path) {
			continue
		}
		if err := s.addExternalFile(&s.result.NativeSources, path, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) addExternalFile(usage *FileUsage, path string, required bool) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat referenced storage file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("referenced storage path is not a regular file: %s", path)
	}
	return s.addFile(usage, path, info)
}

func (s *scanner) addFile(usage *FileUsage, path string, info os.FileInfo) error {
	identity, physicalBytes, err := physicalFile(path, info)
	if err != nil {
		return err
	}
	usage.Files++
	usage.ApparentBytes += info.Size()
	s.result.TotalFiles++
	s.result.TotalApparentBytes += info.Size()
	if _, exists := s.physicalFiles[identity]; exists {
		s.result.HardlinkAliases++
		return nil
	}
	s.physicalFiles[identity] = struct{}{}
	usage.PhysicalBytes += physicalBytes
	s.result.UniquePhysicalFiles++
	s.result.TotalPhysicalBytes += physicalBytes
	return nil
}

func decodeManifest(path string) (manifestRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return manifestRecord{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var manifest manifestRecord
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifestRecord{}, fmt.Errorf("decode manifest %s: %w", path, err)
	}
	return manifest, nil
}

func walkIfPresent(root string, walk fs.WalkDirFunc) error {
	err := filepath.WalkDir(root, walk)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func cleanAbsolutePath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(path)
}

func cleanPathWithin(root string, path string) (string, error) {
	clean := cleanAbsolutePath(path)
	if !pathWithin(root, clean) {
		return "", errors.New("path escapes its managed root")
	}
	return clean, nil
}

func pathWithin(root string, path string) bool {
	root = cleanAbsolutePath(root)
	path = cleanAbsolutePath(path)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func packGeneration(store string, path string) (string, bool) {
	relative, err := filepath.Rel(filepath.Join(store, "packs"), path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) < 2 || parts[0] == "" || strings.HasPrefix(parts[0], ".") {
		return "", false
	}
	return parts[0], true
}

func isSessionGenerationData(store string, path string) bool {
	relative, err := filepath.Rel(filepath.Join(store, "fs", "sessions"), path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 {
		return false
	}
	name := parts[1]
	return strings.HasPrefix(name, "delta-") && strings.HasSuffix(name, ".jsonl") ||
		strings.HasPrefix(name, "backing-") && strings.HasSuffix(name, ".jsonl") || name == "delta.jsonl"
}

func isCurrentFallback(path string) bool {
	switch filepath.Base(path) {
	case "fallback-current.jsonl", "quarantine-current.jsonl":
		return true
	default:
		return false
	}
}

func isUnownedTemporary(path string) bool {
	name := filepath.Base(path)
	if !strings.HasPrefix(name, ".") {
		return false
	}
	return strings.Contains(name, ".tmp") || strings.HasPrefix(name, ".generation-") ||
		strings.HasPrefix(name, ".compact-") || strings.HasPrefix(name, ".object-") ||
		strings.HasPrefix(name, ".manifest-") || strings.HasPrefix(name, ".materialize-") ||
		strings.HasPrefix(name, ".backing-") || strings.HasPrefix(name, ".CURRENT-")
}

func addInt64(left int64, right int64) (int64, bool) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, true
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, true
	}
	return left + right, false
}
