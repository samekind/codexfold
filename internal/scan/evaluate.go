package scan

import (
	"context"
	"errors"
	"fmt"
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
	ErrSelectorRequired = errors.New("scan requires --all, --search, or at least one session id")
	ErrAllConflict      = errors.New("scan --all cannot be combined with --search or session ids")
	ErrIndexExists      = errors.New("scan index already exists; use --overwrite-index or another path")
	ErrSessionNotFound  = errors.New("Codex session not found")
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
	IndexPath           string                   `json:"index_path"`
	IndexTemporary      bool                     `json:"index_temporary"`
	IndexBytes          int64                    `json:"index_bytes"`
	SessionCount        int                      `json:"session_count"`
	MissingSessionCount int                      `json:"missing_session_count"`
	ChangedSessionCount int                      `json:"changed_session_count"`
	DurationMillis      int64                    `json:"duration_ms"`
	Scan                DedupFileStats           `json:"scan"`
	Layers              []LayerStats             `json:"layers"`
	TopObjects          map[string][]ObjectStats `json:"top_objects,omitempty"`
	Sessions            []EvaluatedSession       `json:"sessions"`
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
	} else if err := prepareIndexPath(indexPath, options.OverwriteIndex); err != nil {
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
		file, err := os.Open(current.session.RolloutPath)
		if err != nil {
			return Result{}, fmt.Errorf("open rollout %s: %w", current.session.ID, err)
		}
		fileStats, scanErr := scanDedupStream(io.LimitReader(file, current.snapshot.size), DedupScanOptions{
			RecordLayer:      layers[LayerRecord],
			FieldLayer:       layers[LayerField],
			CDCLayer:         layers[LayerCDC],
			MinFieldBytes:    options.MinFieldBytes,
			MaxJSONLineBytes: options.MaxJSONLineBytes,
			CDC:              options.CDC,
		}, index)
		closeErr := file.Close()
		if scanErr != nil {
			return Result{}, fmt.Errorf("scan rollout %s: %w", current.session.ID, scanErr)
		}
		if closeErr != nil {
			return Result{}, fmt.Errorf("close rollout %s: %w", current.session.ID, closeErr)
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
		addFileStats(&result.Scan, fileStats)
		result.Sessions = append(result.Sessions, EvaluatedSession{
			SessionID:         current.session.ID,
			RolloutPath:       current.session.RolloutPath,
			Bytes:             current.snapshot.size,
			BytesAfter:        after.size,
			Archived:          current.session.Archived,
			ChangedDuringScan: changed,
		})
		if options.Progress != nil {
			options.Progress(position+1, len(candidates), current.session)
		}
	}
	result.SessionCount = len(result.Sessions)

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

func prepareIndexPath(path string, overwrite bool) error {
	if _, err := os.Stat(path); err == nil && !overwrite {
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
