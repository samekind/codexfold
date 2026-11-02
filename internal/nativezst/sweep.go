package nativezst

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codexzstd"
	"github.com/samekind/codexfold/internal/fold"
)

// NativeFile is one folded or foldable file found under the native root.
type NativeFile struct {
	Path   string
	Info   os.FileInfo
	Marker Marker
	Folded bool
}

// Scan is a walk of the native session tree.
type Scan struct {
	Candidates []NativeFile
	Folded     []NativeFile
	// References maps a manifest key to the folded files that use it.
	References map[string][]string
	// Unmarked indexes unfolded .zst files by inode, so the sweep can find a
	// file whose marker was lost.
	Unmarked  map[uint64]NativeFile
	Footprint Footprint
}

// Footprint is what the native tree holds, decoded and on disk.
type Footprint struct {
	Files              int   `json:"files"`
	LogicalBytes       int64 `json:"logical_bytes"`
	PhysicalBytes      int64 `json:"physical_bytes"`
	CompressedFiles    int   `json:"compressed_files"`
	CompressedBytes    int64 `json:"compressed_bytes"`
	FoldedFiles        int   `json:"folded_files"`
	FoldedLogicalBytes int64 `json:"folded_logical_bytes"`
	FoldedEncodedBytes int64 `json:"folded_encoded_bytes"`
}

// ScanNative walks sessions/ and archived_sessions/ under root. A marker that
// cannot be read fails the scan: a sweep must never act on a partial view.
func ScanNative(ctx context.Context, root string) (Scan, error) {
	scan := Scan{References: make(map[string][]string), Unmarked: make(map[uint64]NativeFile)}
	for _, directory := range []string{"sessions", "archived_sessions"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, fs.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), "._") {
				return nil
			}
			info, err := entry.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			scan.Footprint.Files++
			scan.Footprint.PhysicalBytes += allocatedBytes(info)
			if !IsCandidateName(entry.Name()) {
				scan.Footprint.LogicalBytes += info.Size()
				return nil
			}
			marker, folded, err := ReadMarker(path)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			file := NativeFile{Path: path, Info: info, Marker: marker, Folded: folded}
			if folded {
				key, _ := ManifestKey(marker.Manifest)
				scan.References[key] = append(scan.References[key], path)
				scan.Folded = append(scan.Folded, file)
				scan.Footprint.FoldedFiles++
				scan.Footprint.FoldedLogicalBytes += marker.ContentBytes
				scan.Footprint.FoldedEncodedBytes += marker.EncodedBytes
				scan.Footprint.LogicalBytes += marker.ContentBytes
				return nil
			}
			scan.Candidates = append(scan.Candidates, file)
			scan.Unmarked[inode(info)] = file
			scan.Footprint.CompressedFiles++
			scan.Footprint.CompressedBytes += info.Size()
			scan.Footprint.LogicalBytes += decodedSize(path, info)
			return nil
		})
		if err != nil {
			return Scan{}, err
		}
	}
	return scan, nil
}

func decodedSize(path string, info os.FileInfo) int64 {
	file, err := os.Open(path)
	if err != nil {
		return info.Size()
	}
	defer file.Close()
	if size, err := codexzstd.ContentSize(file); err == nil {
		return size
	}
	return info.Size()
}

// Manifests younger than this may belong to a fold that has not converted
// its file yet; a key must also be missing from two scans this far apart
// before its content is released.
const sweepGrace = 15 * time.Minute

type sweepState struct {
	Version int              `json:"version"`
	Missing map[string]int64 `json:"missing"`
}

type SweepResult struct {
	Manifests  int `json:"manifests"`
	Referenced int `json:"referenced"`
	Pending    int `json:"pending"`
	Repaired   int `json:"repaired"`
	Released   int `json:"released"`
}

// Sweep releases manifests no folded file refers to any more: Codex deleted
// the file, or a restore made it real again. The released content leaves the
// store with the next pack build and GC.
func Sweep(ctx context.Context, store string, scan Scan, now time.Time) (SweepResult, error) {
	var result SweepResult
	entries, err := os.ReadDir(ManifestRoot(store))
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	statePath := filepath.Join(StateRoot(store), "sweep.json")
	state := sweepState{Version: 1, Missing: make(map[string]int64)}
	if data, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(data, &state)
		if state.Missing == nil {
			state.Missing = make(map[string]int64)
		}
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		if _, err := ManifestKey(manifestRelative(key)); err != nil {
			continue
		}
		seen[key] = true
		result.Manifests++
		if len(scan.References[key]) != 0 {
			result.Referenced++
			delete(state.Missing, key)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < sweepGrace {
			result.Pending++
			continue
		}
		if repaired, err := repairMarker(store, key, scan); err != nil {
			return result, err
		} else if repaired {
			result.Repaired++
			delete(state.Missing, key)
			continue
		}
		first, missing := state.Missing[key]
		if !missing {
			state.Missing[key] = now.Unix()
			result.Pending++
			continue
		}
		if now.Sub(time.Unix(first, 0)) < sweepGrace {
			result.Pending++
			continue
		}
		if err := os.Remove(filepath.Join(ManifestRoot(store), entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return result, err
		}
		_ = os.Remove(journalPath(store, key))
		delete(state.Missing, key)
		result.Released++
	}
	for key := range state.Missing {
		if !seen[key] {
			delete(state.Missing, key)
		}
	}
	return result, writeDurableJSON(statePath, state)
}

// repairMarker restores the marker of a file that was converted but lost its
// extended attribute: its inode is still in the tree, empty and the right size.
func repairMarker(store string, key string, scan Scan) (bool, error) {
	data, err := os.ReadFile(journalPath(store, key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record journalRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Key != key {
		return false, nil
	}
	file, found := scan.Unmarked[record.Inode]
	if !found || file.Info.Size() != record.EncodedBytes || allocatedBytes(file.Info) != 0 {
		return false, nil
	}
	manifest, err := fold.LoadManifestPath(filepath.Join(ManifestRoot(store), key+".json"))
	if err != nil {
		return false, err
	}
	marker := Marker{
		Version: MarkerVersion, Kind: MarkerKind, Profile: codexzstd.Profile, Manifest: manifestRelative(key),
		EncodedBytes: record.EncodedBytes, EncodedSHA256: record.EncodedSHA256,
		ContentBytes: manifest.Source.Bytes, ContentSHA256: manifest.Source.SHA256,
	}
	handle, err := os.OpenFile(file.Path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer handle.Close()
	if err := writeMarker(handle, marker); err != nil {
		return false, err
	}
	return true, durableSync(handle)
}
