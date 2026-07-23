package pack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

type BuildOptions struct {
	BlockBytes    int64
	PackBytes     int64
	Budget        storage.Checker
	BeforePublish func() error
}

type BuildResult struct {
	Generation  string                      `json:"generation"`
	ObjectCount int                         `json:"object_count"`
	BlockCount  int                         `json:"block_count"`
	PackCount   int                         `json:"pack_count"`
	RawBytes    int64                       `json:"raw_bytes"`
	StoredBytes int64                       `json:"stored_bytes"`
	Storage     *storage.MutationAccounting `json:"storage,omitempty"`
}

type packWriter struct {
	directory string
	limit     int64
	sequence  int
	file      *os.File
	name      string
	offset    int64
}

func Build(ctx context.Context, storeDir string, options BuildOptions) (BuildResult, error) {
	if storeDir == "" {
		return BuildResult{}, errors.New("pack store directory is required")
	}
	if options.BlockBytes <= 0 {
		options.BlockBytes = defaultBlockBytes
	}
	if options.PackBytes <= 0 {
		options.PackBytes = defaultPackBytes
	}
	if options.BlockBytes > int64(int(^uint(0)>>1)) {
		return BuildResult{}, errors.New("pack block size exceeds platform integer size")
	}
	refs, err := referencedObjects(storeDir)
	if err != nil {
		return BuildResult{}, err
	}
	budget := options.Budget
	if budget == nil {
		guard, err := storage.DefaultGuard(storeDir)
		if err != nil {
			return BuildResult{}, err
		}
		budget = guard
	}
	estimatedBytes, err := estimatedGenerationBytes(refs)
	if err != nil {
		return BuildResult{}, err
	}
	storageAssessment, err := budget.Check(ctx, storage.Projection{
		Operation:                       "pack-build",
		AdditionalPersistentBytes:       estimatedBytes,
		TemporaryBytes:                  estimatedBytes,
		TemporaryPersistentOverlapBytes: estimatedBytes,
	})
	if err != nil {
		return BuildResult{}, err
	}
	packsDir := filepath.Join(storeDir, "packs")
	if err := os.MkdirAll(packsDir, 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create packs directory: %w", err)
	}
	temporaryDir, err := os.MkdirTemp(packsDir, ".generation-")
	if err != nil {
		return BuildResult{}, fmt.Errorf("create temporary pack generation: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporaryDir) }()
	generation := "gen-" + strings.TrimPrefix(filepath.Base(temporaryDir), ".generation-")
	index := Index{Version: IndexVersion, Kind: IndexKind, Generation: generation, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), BlockBytes: options.BlockBytes, Objects: make([]Object, 0, len(refs))}
	result := BuildResult{Generation: generation, ObjectCount: len(refs)}
	writer := &packWriter{directory: temporaryDir, limit: options.PackBytes}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return BuildResult{}, fmt.Errorf("create pack encoder: %w", err)
	}
	defer encoder.Close()
	store := fold.NewObjectStore(storeDir)
	buffer := make([]byte, int(options.BlockBytes))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return BuildResult{}, err
		}
		stream, err := store.OpenStream(ref)
		if err != nil {
			return BuildResult{}, err
		}
		object := Object{SHA256: ref.SHA256, RawBytes: ref.RawBytes}
		objectHash := sha256.New()
		var rawOffset int64
		for {
			n, readErr := io.ReadFull(stream, buffer)
			if n > 0 {
				raw := buffer[:n]
				_, _ = objectHash.Write(raw)
				compressed := encoder.EncodeAll(raw, nil)
				stored := compressed
				encoding := EncodingZstd
				if preferRawBlock(raw, compressed) {
					stored = raw
					encoding = EncodingRaw
				}
				packName, packOffset, writeErr := writer.write(stored)
				if writeErr != nil {
					_ = stream.Close()
					return BuildResult{}, writeErr
				}
				blockHash := sha256.Sum256(raw)
				object.Blocks = append(object.Blocks, Block{Pack: packName, PackOffset: packOffset, StoredBytes: int64(len(stored)), RawOffset: rawOffset, RawBytes: int64(n), SHA256: hex.EncodeToString(blockHash[:]), Encoding: encoding})
				rawOffset += int64(n)
				result.BlockCount++
				result.RawBytes += int64(n)
				result.StoredBytes += int64(len(stored))
			}
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				break
			}
			if readErr != nil {
				_ = stream.Close()
				return BuildResult{}, fmt.Errorf("read loose object %s: %w", ref.SHA256, readErr)
			}
		}
		if err := stream.Close(); err != nil {
			return BuildResult{}, err
		}
		if rawOffset != ref.RawBytes || hex.EncodeToString(objectHash.Sum(nil)) != ref.SHA256 {
			return BuildResult{}, fmt.Errorf("loose object %s failed stream verification", ref.SHA256)
		}
		index.Objects = append(index.Objects, object)
	}
	if err := writer.close(); err != nil {
		return BuildResult{}, err
	}
	result.PackCount = writer.sequence
	if err := writeIndex(temporaryDir, index); err != nil {
		return BuildResult{}, err
	}
	if err := verifyGeneration(ctx, temporaryDir, index); err != nil {
		return BuildResult{}, fmt.Errorf("verify candidate pack generation: %w", err)
	}
	finalDir := filepath.Join(packsDir, generation)
	if err := os.Rename(temporaryDir, finalDir); err != nil {
		return BuildResult{}, fmt.Errorf("publish pack generation directory: %w", err)
	}
	if err := syncDirectory(packsDir); err != nil {
		return BuildResult{}, err
	}
	if options.BeforePublish != nil {
		if err := options.BeforePublish(); err != nil {
			return BuildResult{}, err
		}
	}
	if err := publishCurrent(packsDir, generation); err != nil {
		return BuildResult{}, err
	}
	result.Storage = storage.CompleteAccounting(ctx, storageAssessment, storeDir)
	return result, nil
}

func preferRawBlock(raw []byte, compressed []byte) bool {
	if len(compressed) >= len(raw) {
		return true
	}
	return len(raw) <= 16<<10 && len(compressed)*2 >= len(raw)
}

func estimatedGenerationBytes(refs []fold.ObjectRef) (int64, error) {
	const fixedOverhead = int64(1 << 20)
	var rawBytes int64
	for _, ref := range refs {
		if ref.RawBytes < 0 || rawBytes > math.MaxInt64-ref.RawBytes {
			return 0, errors.New("pack generation byte estimate overflow")
		}
		rawBytes += ref.RawBytes
	}
	compressionOverhead := rawBytes/16 + fixedOverhead
	if rawBytes > math.MaxInt64-compressionOverhead {
		return 0, errors.New("pack generation byte estimate overflow")
	}
	return rawBytes + compressionOverhead, nil
}

func referencedObjects(storeDir string) ([]fold.ObjectRef, error) {
	refs := make(map[string]fold.ObjectRef)
	err := filepath.WalkDir(filepath.Join(storeDir, "manifests"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		manifest, err := fold.LoadManifestPath(path)
		if err != nil {
			return err
		}
		for _, part := range manifest.Parts {
			if existing, ok := refs[part.Object.SHA256]; ok && existing.RawBytes != part.Object.RawBytes {
				return fmt.Errorf("object %s has conflicting raw lengths", part.Object.SHA256)
			}
			refs[part.Object.SHA256] = part.Object
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read manifests for pack build: %w", err)
	}
	digests := make([]string, 0, len(refs))
	for digest := range refs {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	result := make([]fold.ObjectRef, 0, len(digests))
	for _, digest := range digests {
		result = append(result, refs[digest])
	}
	return result, nil
}

func (w *packWriter) write(data []byte) (string, int64, error) {
	if w.file == nil || (w.offset > 0 && w.offset+int64(len(data)) > w.limit) {
		if err := w.rotate(); err != nil {
			return "", 0, err
		}
	}
	offset := w.offset
	if _, err := w.file.Write(data); err != nil {
		return "", 0, fmt.Errorf("write pack file: %w", err)
	}
	w.offset += int64(len(data))
	return w.name, offset, nil
}

func (w *packWriter) rotate() error {
	if err := w.closeCurrent(); err != nil {
		return err
	}
	w.sequence++
	w.name = fmt.Sprintf("pack-%06d.pack", w.sequence)
	file, err := os.OpenFile(filepath.Join(w.directory, w.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create pack file: %w", err)
	}
	w.file = file
	w.offset = 0
	return nil
}

func (w *packWriter) closeCurrent() error {
	if w.file == nil {
		return nil
	}
	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		return fmt.Errorf("sync pack file: %w", err)
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close pack file: %w", err)
	}
	w.file = nil
	return nil
}

func (w *packWriter) close() error { return w.closeCurrent() }

func writeIndex(directory string, index Index) error {
	data, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("encode pack index: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(directory, "index.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create pack index: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write pack index: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync pack index: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close pack index: %w", err)
	}
	return syncDirectory(directory)
}

func publishCurrent(packsDir string, generation string) error {
	temporary, err := os.CreateTemp(packsDir, ".CURRENT-")
	if err != nil {
		return fmt.Errorf("create temporary CURRENT: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.WriteString(generation + "\n"); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write CURRENT: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync CURRENT: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close CURRENT: %w", err)
	}
	if err := replaceFile(temporaryPath, filepath.Join(packsDir, "CURRENT")); err != nil {
		return fmt.Errorf("publish CURRENT: %w", err)
	}
	return syncDirectory(packsDir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync %s: %w", path, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", path, err)
	}
	return nil
}
