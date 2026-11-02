package nativezst

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
	"strconv"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/samekind/codexfold/internal/codexzstd"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

// Prepared is a .zst whose content is folded and proven, not yet converted.
type Prepared struct {
	Path         string
	Info         os.FileInfo
	Marker       Marker
	ManifestPath string
	// NewStoredBytes is what the fold added to the store; content already in
	// the store (a managed copy of the same session) costs nothing.
	NewStoredBytes int64
}

// Prepare folds the decoded content of a Codex .zst into the store and proves
// that the store re-encodes it to the file's exact bytes. The file itself is
// left untouched; Discard undoes a Prepare that is not converted.
//
// Prepare is built for a nearly full disk: the content is folded straight
// from the decoder, never staged, and when free space is short a dry run
// prices the fold exactly, so a file whose content the store already holds
// costs almost nothing.
func Prepare(ctx context.Context, source *Source, path string) (Prepared, error) {
	if !supported() {
		return Prepared{}, ErrUnsupported
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Prepared{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return Prepared{}, fmt.Errorf("%s is not a non-empty regular file", path)
	}
	if _, folded, err := ReadMarker(path); err != nil || folded {
		return Prepared{}, errors.Join(err, errors.New("native .zst is already folded"))
	}
	proof, err := codexzstd.Verify(ctx, path)
	if err != nil {
		return Prepared{}, err
	}
	store := source.Store()
	guard, err := storage.DefaultGuard(store)
	if err != nil {
		return Prepared{}, err
	}
	existing, release, err := source.ExistingObjects()
	if err != nil {
		return Prepared{}, err
	}
	defer release()
	options := fold.FoldOptions{StoreDir: store, ExistingReader: existing}
	// The dry run writes nothing. It prices the fold exactly for the
	// free-space check, and keeps a small rollout the store shares nothing
	// with as Codex's .zst: its manifest and index would outweigh the bytes
	// freed.
	dry, err := foldDecoded(ctx, path, proof, key(proof), options)
	if err != nil {
		return Prepared{}, err
	}
	if foldCost(dry) >= info.Size()*worthwhileShare/100 {
		return Prepared{}, ErrNotWorthwhile
	}
	options.NewBytesEstimate = max(dry.NewStoredBytes, 1)

	manifestKey := key(proof)
	if err := os.MkdirAll(ManifestRoot(store), 0o755); err != nil {
		return Prepared{}, err
	}
	manifestPath := filepath.Join(ManifestRoot(store), manifestKey+".json")
	options.Apply = true
	options.ManifestPathOverride = manifestPath
	// A full store scan per file cost more than the fold itself; free space
	// is still probed for every file.
	options.Budget = reclaimingGuard{inner: storage.SharedInventoryGuard{Guard: guard}, reclaim: info.Size()}
	result, err := foldDecoded(ctx, path, proof, manifestKey, options)
	if err != nil {
		_ = os.Remove(manifestPath)
		return Prepared{}, err
	}
	prepared := Prepared{
		Path: path, Info: info, ManifestPath: manifestPath, NewStoredBytes: result.NewStoredBytes,
		Marker: Marker{
			Version: MarkerVersion, Kind: MarkerKind, Profile: proof.Profile, Manifest: manifestRelative(manifestKey),
			EncodedBytes: proof.EncodedBytes, EncodedSHA256: proof.EncodedSHA256,
			ContentBytes: proof.ContentBytes, ContentSHA256: proof.ContentSHA256,
		},
	}
	if result.SourceBytes != proof.ContentBytes || result.SourceSHA256 != proof.ContentSHA256 {
		Discard(prepared)
		return Prepared{}, errors.New("folded content does not match the decoded .zst")
	}
	if err := proveServed(ctx, source, prepared.Marker); err != nil {
		Discard(prepared)
		return Prepared{}, err
	}
	if after, err := os.Lstat(path); err != nil || !sameVersion(info, after) {
		Discard(prepared)
		return Prepared{}, errors.Join(err, ErrChanged)
	}
	return prepared, nil
}

func key(proof codexzstd.Proof) string {
	return "zst-" + proof.EncodedSHA256[:24] + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// foldDecoded folds the file's decoded content as it streams out of the
// decoder. Prepare compares the result with the proof, so a file changed
// after verification is never folded silently.
func foldDecoded(ctx context.Context, path string, proof codexzstd.Proof, id string, options fold.FoldOptions) (fold.FoldResult, error) {
	input, err := os.Open(path)
	if err != nil {
		return fold.FoldResult{}, err
	}
	defer input.Close()
	decoder, err := zstd.NewReader(input, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(1<<31))
	if err != nil {
		return fold.FoldResult{}, err
	}
	defer decoder.Close()
	options.Content = &contextReader{ctx: ctx, reader: decoder}
	options.ContentBytes = proof.ContentBytes
	return fold.Fold(ctx, fold.Session{ID: id, RolloutPath: path, Archived: true}, options)
}

// ErrNotWorthwhile means folding the file would not free space: the store
// shares too little of its content to beat Codex's own compression.
var ErrNotWorthwhile = errors.New("folding this .zst would not save space")

// A fold must cost less than this share of the file it frees.
const worthwhileShare = 80

// foldCost bounds what a fold occupies once packed: its new objects, plus a
// manifest part and pack index records for each, with the manifest stored
// again in the pack's recovery archive.
func foldCost(dry fold.FoldResult) int64 {
	const perPart, perObject, fixed = 3 * 160, 256, 8 << 10
	return dry.NewStoredBytes + int64(dry.PartCount)*perPart + int64(dry.UniqueObjects)*perObject + fixed
}

const (
	// A fold this small may run below the free-space reserve when its
	// conversion frees at least reclaimRatio times as much, down to
	// emergencyFloor: that is how the store digs itself out of a full disk.
	emergencyPeakBytes = 64 << 20
	reclaimRatio       = 4
	emergencyFloor     = 1 << 30
)

type reclaimingGuard struct {
	inner   storage.Checker
	reclaim int64
}

func (g reclaimingGuard) Check(ctx context.Context, projection storage.Projection) (storage.Assessment, error) {
	assessment, err := g.inner.Check(ctx, projection)
	if err == nil || !errors.Is(err, storage.ErrBudgetExceeded) {
		return assessment, err
	}
	report := assessment.Budget
	if len(report.Rejections) != 1 || report.Rejections[0] != storage.RejectionFreeSpaceReserve {
		return assessment, err
	}
	peak := projection.AdditionalPersistentBytes + projection.TemporaryBytes - projection.TemporaryPersistentOverlapBytes
	if peak > emergencyPeakBytes || g.reclaim < reclaimRatio*peak || report.FreeSpaceAfterPeak < emergencyFloor {
		return assessment, err
	}
	assessment.Budget.Allowed = true
	assessment.Budget.Rejections = nil
	return assessment, nil
}

// Discard removes the manifest of a Prepare that will not be converted. Its
// objects become unreferenced and leave with the next pack and GC.
func Discard(prepared Prepared) {
	if prepared.ManifestPath != "" {
		_ = os.Remove(prepared.ManifestPath)
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// proveServed reads the file exactly as the mount will serve it.
func proveServed(ctx context.Context, source *Source, marker Marker) error {
	reader, err := source.Open(marker)
	if err != nil {
		return err
	}
	defer reader.Close()
	hasher := sha256.New()
	buffer := make([]byte, 1<<20)
	var offset int64
	for offset < marker.EncodedBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.ReadAt(buffer, offset)
		_, _ = hasher.Write(buffer[:n])
		offset += int64(n)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("re-encode folded .zst: %w", err)
		}
		if n == 0 {
			break
		}
	}
	if offset != marker.EncodedBytes || hex.EncodeToString(hasher.Sum(nil)) != marker.EncodedSHA256 {
		return errors.New("the store does not reproduce the original .zst bytes")
	}
	return nil
}

func sameVersion(before os.FileInfo, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) &&
		changeTime(before).Equal(changeTime(after))
}

type journalRecord struct {
	Version       int    `json:"version"`
	Key           string `json:"key"`
	Path          string `json:"path"`
	Inode         uint64 `json:"inode"`
	EncodedBytes  int64  `json:"encoded_bytes"`
	EncodedSHA256 string `json:"encoded_sha256"`
	FoldedAt      string `json:"folded_at"`
}

func journalPath(store string, key string) string {
	return filepath.Join(StateRoot(store), "journal", key+".json")
}

// Convert binds the file to its manifest and frees its data blocks in place.
// The caller must hold the mount's namespace lock and know that no handle is
// open on the file. Size, inode, mode and times are kept.
func Convert(store string, prepared Prepared) error {
	key, err := ManifestKey(prepared.Marker.Manifest)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(prepared.Path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil {
		return err
	}
	if !sameVersion(prepared.Info, current) {
		return ErrChanged
	}
	// The journal outlives a marker lost to a crash: the sweep finds the
	// inode again and restores the marker instead of releasing the content.
	record := journalRecord{
		Version: 1, Key: key, Path: prepared.Path, Inode: inode(current),
		EncodedBytes: prepared.Marker.EncodedBytes, EncodedSHA256: prepared.Marker.EncodedSHA256,
		FoldedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeDurableJSON(journalPath(store, key), record); err != nil {
		return err
	}
	accessTime, modifyTime := fileTimes(current)
	if err := writeMarker(file, prepared.Marker); err != nil {
		_ = os.Remove(journalPath(store, key))
		return err
	}
	if err := durableSync(file); err != nil {
		if removeMarker(file) == nil {
			_ = os.Remove(journalPath(store, key))
			return err
		}
		return errors.Join(err, ErrMarkerKept)
	}
	// From here the file is bound to its manifest: whatever happens, the
	// manifest must stay.
	if err := releaseBlocks(file, current.Size()); err != nil {
		return errors.Join(err, ErrMarkerKept)
	}
	if err := durableSync(file); err != nil {
		return errors.Join(err, ErrMarkerKept)
	}
	if err := os.Chtimes(prepared.Path, accessTime, modifyTime); err != nil {
		return errors.Join(err, ErrMarkerKept)
	}
	return nil
}

// Prove re-encodes the stored content exactly as the mount serves it and
// checks it against the marker.
func Prove(ctx context.Context, source *Source, marker Marker) error {
	return proveServed(ctx, source, marker)
}

// FinishRelease completes a conversion interrupted after its marker was
// written. Call Prove first, outside the caller's namespace lock.
func FinishRelease(path string, info os.FileInfo) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil {
		return err
	}
	if !sameVersion(info, current) {
		return ErrChanged
	}
	if _, folded, err := ReadMarker(path); err != nil || !folded {
		return errors.Join(err, ErrChanged)
	}
	accessTime, modifyTime := fileTimes(current)
	if err := releaseBlocks(file, current.Size()); err != nil {
		return err
	}
	if err := durableSync(file); err != nil {
		return err
	}
	return os.Chtimes(path, accessTime, modifyTime)
}

// Restore writes the original .zst bytes back into a folded file and removes
// its marker. Until the marker is gone the mount keeps serving from the
// store, so an interrupted restore is still read correctly and can be rerun.
func Restore(ctx context.Context, source *Source, path string) error {
	marker, folded, err := ReadMarker(path)
	if err != nil {
		return err
	}
	if !folded {
		return fmt.Errorf("%s is not a folded .zst", path)
	}
	reader, err := source.Open(marker)
	if err != nil {
		return err
	}
	defer reader.Close()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != marker.EncodedBytes {
		return fmt.Errorf("%s is %d bytes, its marker records %d", path, info.Size(), marker.EncodedBytes)
	}
	accessTime, modifyTime := fileTimes(info)
	hasher := sha256.New()
	buffer := make([]byte, 1<<20)
	var offset int64
	for offset < marker.EncodedBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := reader.ReadAt(buffer, offset)
		if n > 0 {
			if _, err := file.WriteAt(buffer[:n], offset); err != nil {
				return err
			}
			_, _ = hasher.Write(buffer[:n])
			offset += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if n == 0 {
			break
		}
	}
	if offset != marker.EncodedBytes || hex.EncodeToString(hasher.Sum(nil)) != marker.EncodedSHA256 {
		return errors.New("restored bytes do not match the folded .zst")
	}
	if err := durableSync(file); err != nil {
		return err
	}
	if err := removeMarker(file); err != nil {
		return err
	}
	if err := durableSync(file); err != nil {
		return err
	}
	return os.Chtimes(path, accessTime, modifyTime)
}

func writeDurableJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.json")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := durableSync(temporary); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	return errors.Join(syncErr, directory.Close())
}
