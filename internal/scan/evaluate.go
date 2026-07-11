package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jstar0/codexfold/internal/codex"
)

const (
	LayerRecord = dedupLayerRecord
	LayerField  = dedupLayerField
	LayerCDC    = dedupLayerCDC
)

var (
	ErrSelectorRequired     = errors.New("scan requires --all, --search, or at least one session id")
	ErrAllConflict          = errors.New("scan --all cannot be combined with --search or session ids")
	ErrIndexExists          = errors.New("scan index already exists; use --overwrite-index or another path")
	ErrIndexRebuildRequired = errors.New("scan index rebuild required; use --overwrite-index")
	ErrSessionNotFound      = errors.New("Codex session not found")
)

type LayerStats = DedupLayerStats
type ObjectStats = DedupObjectStats

type Options struct {
	SessionIDs       []string
	Search           string
	All              bool
	ExcludeArchived  bool
	Limit            int
	MaxBytes         int64
	IndexPath        string
	OverwriteIndex   bool
	Incremental      bool
	Layers           []string
	MinFieldBytes    int64
	MaxJSONLineBytes int64
	CDC              DedupCDCOptions
	Top              int
	Progress         func(completed int, total int, session codex.Session)
}

type EvaluatedSession struct {
	SessionID         string `json:"session_id"`
	RolloutPath       string `json:"rollout_path"`
	Bytes             int64  `json:"bytes"`
	BytesAfter        int64  `json:"bytes_after"`
	Archived          bool   `json:"archived"`
	ChangedDuringScan bool   `json:"changed_during_scan"`
}

type Result struct {
	IndexPath            string                   `json:"index_path"`
	IndexTemporary       bool                     `json:"index_temporary"`
	IndexBytes           int64                    `json:"index_bytes"`
	SessionCount         int                      `json:"session_count"`
	MissingSessionCount  int                      `json:"missing_session_count"`
	ChangedSessionCount  int                      `json:"changed_session_count"`
	SkippedSessionCount  int                      `json:"skipped_session_count"`
	AppendedSessionCount int                      `json:"appended_session_count"`
	ProcessedBytes       int64                    `json:"processed_bytes"`
	DurationMillis       int64                    `json:"duration_ms"`
	Scan                 DedupFileStats           `json:"scan"`
	Layers               []LayerStats             `json:"layers"`
	TopObjects           map[string][]ObjectStats `json:"top_objects,omitempty"`
	Sessions             []EvaluatedSession       `json:"sessions"`
}

type candidate struct {
	session  codex.Session
	snapshot fileSnapshot
}

type fileSnapshot struct {
	size         int64
	modTimeNanos int64
}

func Evaluate(ctx context.Context, sessions []codex.Session, options Options) (Result, error) {
	if err := validateOptions(options); err != nil {
		return Result{}, err
	}
	layers, err := normalizeLayers(options.Layers)
	if err != nil {
		return Result{}, err
	}
	candidates, missing, err := selectCandidates(sessions, options)
	if err != nil {
		return Result{}, err
	}

	indexPath := options.IndexPath
	temporary := false
	var temporaryDir string
	if indexPath == "" {
		temporaryDir, err = os.MkdirTemp("", "codexfold-")
		if err != nil {
			return Result{}, fmt.Errorf("create temporary scan directory: %w", err)
		}
		temporary = true
		indexPath = filepath.Join(temporaryDir, "scan.sqlite")
		defer func() { _ = os.RemoveAll(temporaryDir) }()
	} else if err := prepareIndexPath(indexPath, options.OverwriteIndex, options.Incremental); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create scan index directory: %w", err)
	}

	index, err := openDedupIndex(indexPath)
	if err != nil {
		return Result{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = index.Close()
		}
	}()
	options = normalizeScanOptions(options)
	if err := index.EnsureConfiguration(scanConfiguration(layers, options)); err != nil {
		return Result{}, err
	}

	started := time.Now()
	result := Result{
		IndexPath:           indexPath,
		IndexTemporary:      temporary,
		MissingSessionCount: missing,
		TopObjects:          make(map[string][]ObjectStats),
		Sessions:            make([]EvaluatedSession, 0, len(candidates)),
	}
	for position, current := range candidates {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		state, exists, err := index.LoadFileState(current.session.RolloutPath)
		if err != nil {
			return Result{}, err
		}
		if options.Incremental && exists && current.snapshot.size == state.Size && current.snapshot.modTimeNanos == state.ModTimeNanos {
			result.SkippedSessionCount++
			result.Sessions = append(result.Sessions, evaluatedSession(current, current.snapshot, false))
			if options.Progress != nil {
				options.Progress(position+1, len(candidates), current.session)
			}
			continue
		}

		var fileStats DedupFileStats
		var digest string
		var endsWithNewline bool
		if options.Incremental && exists {
			if current.snapshot.size <= state.Size {
				return Result{}, fmt.Errorf("%w: rollout %s was rewritten or truncated", ErrIndexRebuildRequired, current.session.ID)
			}
			if !state.EndsWithNewline {
				return Result{}, fmt.Errorf("%w: rollout %s previously ended with a partial JSONL record", ErrIndexRebuildRequired, current.session.ID)
			}
			prefixHasher, prefixDigest, err := hashFilePrefix(current.session.RolloutPath, state.Size)
			if err != nil {
				return Result{}, err
			}
			if prefixDigest != state.PrefixSHA256 {
				return Result{}, fmt.Errorf("%w: rollout %s prefix changed", ErrIndexRebuildRequired, current.session.ID)
			}
			if err := index.BeginFile(current.session.RolloutPath); err != nil {
				return Result{}, err
			}
			fileStats, digest, endsWithNewline, err = scanAppendedFile(ctx, current, state, layers, options, index, prefixHasher)
			if err != nil {
				_ = index.RollbackFile()
				return Result{}, err
			}
			result.AppendedSessionCount++
			result.ProcessedBytes += current.snapshot.size - state.Size
		} else {
			if err := index.BeginFile(current.session.RolloutPath); err != nil {
				return Result{}, err
			}
			if err := index.DeleteFileLayers(layers); err != nil {
				_ = index.RollbackFile()
				return Result{}, err
			}
			fileStats, digest, endsWithNewline, err = scanWholeFile(ctx, current, layers, options, index)
			if err != nil {
				_ = index.RollbackFile()
				return Result{}, err
			}
			result.ProcessedBytes += current.snapshot.size
		}
		if err := index.SaveFileState(scannedFileState{
			Path: current.session.RolloutPath, Size: current.snapshot.size,
			ModTimeNanos: current.snapshot.modTimeNanos, PrefixSHA256: digest,
			EndsWithNewline: endsWithNewline, Stats: fileStats,
		}); err != nil {
			_ = index.RollbackFile()
			return Result{}, err
		}
		if err := index.CommitFile(); err != nil {
			return Result{}, err
		}
		afterInfo, err := os.Stat(current.session.RolloutPath)
		if err != nil {
			return Result{}, fmt.Errorf("stat rollout after scan %s: %w", current.session.ID, err)
		}
		after := snapshotFile(afterInfo)
		changed := snapshotChanged(current.snapshot, after)
		if changed {
			result.ChangedSessionCount++
		}
		result.Sessions = append(result.Sessions, evaluatedSession(current, after, changed))
		if options.Progress != nil {
			options.Progress(position+1, len(candidates), current.session)
		}
	}
	result.SessionCount = len(result.Sessions)
	if options.Incremental && options.All && !options.ExcludeArchived && options.Limit == 0 && options.MaxBytes == 0 {
		paths := make(map[string]struct{}, len(candidates))
		for _, current := range candidates {
			paths[current.session.RolloutPath] = struct{}{}
		}
		if err := index.RemoveFilesNotIn(paths); err != nil {
			return Result{}, err
		}
	}
	result.Scan, err = index.CorpusStats()
	if err != nil {
		return Result{}, err
	}

	layerNames := make([]string, 0, len(layers))
	for layer := range layers {
		layerNames = append(layerNames, layer)
	}
	sort.Strings(layerNames)
	for _, layer := range layerNames {
		stats, err := index.LayerStats(layer)
		if err != nil {
			return Result{}, err
		}
		result.Layers = append(result.Layers, stats)
		top, err := index.TopObjects(layer, options.Top)
		if err != nil {
			return Result{}, err
		}
		if len(top) > 0 {
			result.TopObjects[layer] = top
		}
	}
	if err := index.Close(); err != nil {
		return Result{}, err
	}
	closed = true
	result.IndexBytes = indexDiskBytes(indexPath)
	result.DurationMillis = time.Since(started).Milliseconds()
	return result, nil
}

func scanWholeFile(ctx context.Context, current candidate, layers map[string]bool, options Options, index *dedupIndex) (DedupFileStats, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return DedupFileStats{}, "", false, err
	}
	file, err := os.Open(current.session.RolloutPath)
	if err != nil {
		return DedupFileStats{}, "", false, fmt.Errorf("open rollout %s: %w", current.session.ID, err)
	}
	hasher := sha256.New()
	stats, scanErr := scanDedupStream(io.TeeReader(io.LimitReader(file, current.snapshot.size), hasher), dedupOptions(layers, options), index)
	closeErr := file.Close()
	if scanErr != nil {
		return DedupFileStats{}, "", false, fmt.Errorf("scan rollout %s: %w", current.session.ID, scanErr)
	}
	if closeErr != nil {
		return DedupFileStats{}, "", false, fmt.Errorf("close rollout %s: %w", current.session.ID, closeErr)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if err := verifyScannedPrefix(current.session.RolloutPath, current.snapshot.size, digest); err != nil {
		return DedupFileStats{}, "", false, err
	}
	endsWithNewline, err := filePrefixEndsWithNewline(current.session.RolloutPath, current.snapshot.size)
	if err != nil {
		return DedupFileStats{}, "", false, err
	}
	return stats, digest, endsWithNewline, nil
}

func scanAppendedFile(ctx context.Context, current candidate, previous scannedFileState, layers map[string]bool, options Options, index *dedupIndex, prefixHasher hash.Hash) (DedupFileStats, string, bool, error) {
	file, err := os.Open(current.session.RolloutPath)
	if err != nil {
		return DedupFileStats{}, "", false, fmt.Errorf("open appended rollout %s: %w", current.session.ID, err)
	}
	tailBytes := current.snapshot.size - previous.Size
	tailReader := io.NewSectionReader(file, previous.Size, tailBytes)
	tailLayers := map[string]bool{LayerField: layers[LayerField], LayerRecord: layers[LayerRecord]}
	tailStats, scanErr := scanDedupStream(io.TeeReader(tailReader, prefixHasher), dedupOptions(tailLayers, options), index)
	closeErr := file.Close()
	if scanErr != nil {
		return DedupFileStats{}, "", false, fmt.Errorf("scan appended rollout %s: %w", current.session.ID, scanErr)
	}
	if closeErr != nil {
		return DedupFileStats{}, "", false, fmt.Errorf("close appended rollout %s: %w", current.session.ID, closeErr)
	}
	stats := previous.Stats
	addFileStats(&stats, tailStats)
	stats.ScannedBytes = current.snapshot.size

	if layers[LayerCDC] {
		if err := index.DeleteFileLayers(map[string]bool{LayerCDC: true}); err != nil {
			return DedupFileStats{}, "", false, err
		}
		file, err := os.Open(current.session.RolloutPath)
		if err != nil {
			return DedupFileStats{}, "", false, fmt.Errorf("open rollout for CDC rebuild %s: %w", current.session.ID, err)
		}
		cdcStats, scanErr := scanDedupStream(io.LimitReader(file, current.snapshot.size), dedupOptions(map[string]bool{LayerCDC: true}, options), index)
		closeErr := file.Close()
		if scanErr != nil {
			return DedupFileStats{}, "", false, fmt.Errorf("rebuild CDC rollout %s: %w", current.session.ID, scanErr)
		}
		if closeErr != nil {
			return DedupFileStats{}, "", false, fmt.Errorf("close CDC rollout %s: %w", current.session.ID, closeErr)
		}
		stats.CDCChunkCount = cdcStats.CDCChunkCount
		stats.CDCBytes = cdcStats.CDCBytes
	}
	if err := ctx.Err(); err != nil {
		return DedupFileStats{}, "", false, err
	}
	digest := hex.EncodeToString(prefixHasher.Sum(nil))
	if err := verifyScannedPrefix(current.session.RolloutPath, current.snapshot.size, digest); err != nil {
		return DedupFileStats{}, "", false, err
	}
	endsWithNewline, err := filePrefixEndsWithNewline(current.session.RolloutPath, current.snapshot.size)
	if err != nil {
		return DedupFileStats{}, "", false, err
	}
	return stats, digest, endsWithNewline, nil
}

func dedupOptions(layers map[string]bool, options Options) DedupScanOptions {
	return DedupScanOptions{
		RecordLayer: layers[LayerRecord], FieldLayer: layers[LayerField], CDCLayer: layers[LayerCDC],
		MinFieldBytes: options.MinFieldBytes, MaxJSONLineBytes: options.MaxJSONLineBytes, CDC: options.CDC,
	}
}

func hashFilePrefix(path string, size int64) (hash.Hash, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open rollout prefix: %w", err)
	}
	hasher := sha256.New()
	read, copyErr := io.Copy(hasher, io.LimitReader(file, size))
	closeErr := file.Close()
	if copyErr != nil {
		return nil, "", fmt.Errorf("hash rollout prefix: %w", copyErr)
	}
	if closeErr != nil {
		return nil, "", fmt.Errorf("close rollout prefix: %w", closeErr)
	}
	if read != size {
		return nil, "", fmt.Errorf("%w: rollout prefix has %d bytes, want %d", ErrIndexRebuildRequired, read, size)
	}
	return hasher, hex.EncodeToString(hasher.Sum(nil)), nil
}

func verifyScannedPrefix(path string, size int64, expectedDigest string) error {
	_, digest, err := hashFilePrefix(path, size)
	if err != nil {
		return err
	}
	if digest != expectedDigest {
		return fmt.Errorf("%w: rollout changed while it was scanned", ErrIndexRebuildRequired)
	}
	return nil
}

func filePrefixEndsWithNewline(path string, size int64) (bool, error) {
	if size == 0 {
		return true, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil {
		return false, fmt.Errorf("read rollout final byte: %w", err)
	}
	return last[0] == '\n', nil
}

func evaluatedSession(current candidate, after fileSnapshot, changed bool) EvaluatedSession {
	return EvaluatedSession{
		SessionID: current.session.ID, RolloutPath: current.session.RolloutPath,
		Bytes: current.snapshot.size, BytesAfter: after.size, Archived: current.session.Archived,
		ChangedDuringScan: changed,
	}
}

func normalizeScanOptions(options Options) Options {
	if options.MinFieldBytes <= 0 {
		options.MinFieldBytes = defaultDedupMinFieldBytes
	}
	if options.MaxJSONLineBytes <= 0 {
		options.MaxJSONLineBytes = defaultDedupMaxJSONLineBytes
	}
	if options.CDC.MinBytes <= 0 {
		options.CDC.MinBytes = 64 * 1024
	}
	if options.CDC.AverageBytes <= 0 {
		options.CDC.AverageBytes = 256 * 1024
	}
	if options.CDC.MaxBytes <= 0 {
		options.CDC.MaxBytes = 1024 * 1024
	}
	return options
}

func scanConfiguration(layers map[string]bool, options Options) string {
	value := fmt.Sprintf("layers=%s;field=%d;line=%d;cdc=%d,%d,%d",
		normalizeLayerList(layers), options.MinFieldBytes, options.MaxJSONLineBytes,
		options.CDC.MinBytes, options.CDC.AverageBytes, options.CDC.MaxBytes)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validateOptions(options Options) error {
	if options.All && (len(options.SessionIDs) > 0 || strings.TrimSpace(options.Search) != "") {
		return ErrAllConflict
	}
	if !options.All && len(options.SessionIDs) == 0 && strings.TrimSpace(options.Search) == "" {
		return ErrSelectorRequired
	}
	return nil
}

func normalizeLayers(rawLayers []string) (map[string]bool, error) {
	if len(rawLayers) == 0 {
		rawLayers = []string{LayerField, LayerRecord, LayerCDC}
	}
	result := make(map[string]bool, len(rawLayers))
	for _, raw := range rawLayers {
		for _, layer := range strings.Split(raw, ",") {
			layer = strings.ToLower(strings.TrimSpace(layer))
			if layer == "" {
				continue
			}
			switch layer {
			case LayerField, LayerRecord, LayerCDC:
				result[layer] = true
			default:
				return nil, fmt.Errorf("unknown scan layer %q; expected field, record, or cdc", layer)
			}
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("at least one scan layer is required")
	}
	return result, nil
}

func selectCandidates(sessions []codex.Session, options Options) ([]candidate, int, error) {
	selectedIDs := make(map[string]struct{}, len(options.SessionIDs))
	foundIDs := make(map[string]struct{}, len(options.SessionIDs))
	for _, id := range options.SessionIDs {
		selectedIDs[id] = struct{}{}
	}
	candidates := make([]candidate, 0)
	missing := 0
	for _, session := range sessions {
		if options.ExcludeArchived && session.Archived {
			continue
		}
		if !options.All {
			if len(selectedIDs) > 0 {
				if _, ok := selectedIDs[session.ID]; !ok {
					continue
				}
				foundIDs[session.ID] = struct{}{}
			} else if !matches(session, options.Search) {
				continue
			}
		}
		info, err := os.Stat(session.RolloutPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				missing++
				continue
			}
			return nil, missing, fmt.Errorf("stat rollout %s: %w", session.ID, err)
		}
		if info.Mode().IsRegular() {
			candidates = append(candidates, candidate{session: session, snapshot: snapshotFile(info)})
		}
	}
	for _, id := range options.SessionIDs {
		if _, ok := foundIDs[id]; !ok {
			return nil, missing, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].snapshot.size != candidates[j].snapshot.size {
			return candidates[i].snapshot.size > candidates[j].snapshot.size
		}
		return candidates[i].session.ID < candidates[j].session.ID
	})
	if options.MaxBytes > 0 {
		bounded := make([]candidate, 0, len(candidates))
		var total int64
		for _, current := range candidates {
			if total+current.snapshot.size > options.MaxBytes {
				continue
			}
			bounded = append(bounded, current)
			total += current.snapshot.size
		}
		candidates = bounded
	}
	if options.Limit > 0 && len(candidates) > options.Limit {
		candidates = candidates[:options.Limit]
	}
	return candidates, missing, nil
}

func matches(session codex.Session, query string) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	return needle == "" ||
		strings.Contains(strings.ToLower(session.ID), needle) ||
		strings.Contains(strings.ToLower(session.Title), needle) ||
		strings.Contains(strings.ToLower(session.CWD), needle)
}

func prepareIndexPath(path string, overwrite bool, incremental bool) error {
	if _, err := os.Stat(path); err == nil && !overwrite && !incremental {
		return fmt.Errorf("%w: %s", ErrIndexExists, path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat scan index: %w", err)
	}
	if overwrite {
		for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove old scan index %s: %w", candidate, err)
			}
		}
	}
	return nil
}

func snapshotFile(info os.FileInfo) fileSnapshot {
	return fileSnapshot{size: info.Size(), modTimeNanos: info.ModTime().UnixNano()}
}

func snapshotChanged(before fileSnapshot, after fileSnapshot) bool {
	return before.size != after.size || before.modTimeNanos != after.modTimeNanos
}

func addFileStats(total *DedupFileStats, current DedupFileStats) {
	total.ScannedBytes += current.ScannedBytes
	total.RecordCount += current.RecordCount
	total.ParsedRecordCount += current.ParsedRecordCount
	total.OversizedRecordCount += current.OversizedRecordCount
	total.InvalidJSONRecordCount += current.InvalidJSONRecordCount
	total.FieldCount += current.FieldCount
	total.FieldBytes += current.FieldBytes
	total.CDCChunkCount += current.CDCChunkCount
	total.CDCBytes += current.CDCBytes
}

func indexDiskBytes(path string) int64 {
	var total int64
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(candidate); err == nil {
			total += info.Size()
		}
	}
	return total
}
