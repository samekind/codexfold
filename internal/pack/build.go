package pack

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
	_ "modernc.org/sqlite"
)

type BuildOptions struct {
	BlockBytes    int64
	PackBytes     int64
	Budget        storage.Checker
	BeforePublish func() error
}

type BuildResult struct {
	Generation  string                      `json:"generation"`
	ObjectCount int64                       `json:"object_count"`
	BlockCount  int64                       `json:"block_count"`
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
	lock, err := storage.AcquireOperationLock(storeDir, "objects")
	if err != nil {
		return BuildResult{}, err
	}
	defer lock.Close()
	referenceDirectory, err := os.MkdirTemp(storeDir, ".pack-references-")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(referenceDirectory)
	references, objectCount, rawBytes, err := buildReferenceIndex(ctx, storeDir, filepath.Join(referenceDirectory, "references.sqlite"))
	if err != nil {
		return BuildResult{}, err
	}
	defer references.Close()
	budget := options.Budget
	if budget == nil {
		guard, err := storage.DefaultGuard(storeDir)
		if err != nil {
			return BuildResult{}, err
		}
		budget = guard
	}
	estimatedBytes, err := estimatedGenerationBytes(rawBytes)
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
	result := BuildResult{Generation: generation, ObjectCount: objectCount}
	writer := &packWriter{directory: temporaryDir, limit: options.PackBytes}
	objectsFile, err := os.OpenFile(filepath.Join(temporaryDir, indexV3ObjectsFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return BuildResult{}, err
	}
	blocksFile, err := os.OpenFile(filepath.Join(temporaryDir, indexV3BlocksFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = objectsFile.Close()
		return BuildResult{}, err
	}
	defer objectsFile.Close()
	defer blocksFile.Close()
	encoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true),
	)
	if err != nil {
		return BuildResult{}, fmt.Errorf("create pack encoder: %w", err)
	}
	defer encoder.Close()
	store := fold.NewObjectStore(storeDir)
	var packedSource *Resolver
	if _, err := CurrentGeneration(storeDir); err == nil {
		packedSource, err = Open(storeDir, OpenOptions{CacheBytes: -1})
		if err != nil {
			return BuildResult{}, err
		}
		defer packedSource.Close()
	}
	buffer := make([]byte, int(options.BlockBytes))
	rows, err := references.QueryContext(ctx, `select digest, raw_bytes from pack_references order by digest`)
	if err != nil {
		_ = objectsFile.Close()
		_ = blocksFile.Close()
		return BuildResult{}, err
	}
	packIndexes := make(map[string]uint32)
	packNames := make([]string, 0)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return BuildResult{}, err
		}
		var ref fold.ObjectRef
		if err := rows.Scan(&ref.SHA256, &ref.RawBytes); err != nil {
			_ = rows.Close()
			return BuildResult{}, err
		}
		stream, err := store.OpenStream(ref)
		if err != nil && errors.Is(err, os.ErrNotExist) && packedSource != nil {
			stream, err = packedSource.OpenObject(ctx, ref)
		}
		if err != nil {
			return BuildResult{}, err
		}
		objectHash := sha256.New()
		var rawOffset int64
		firstBlock := result.BlockCount
		var objectBlocks uint32
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
				packIndex, exists := packIndexes[packName]
				if !exists {
					packIndex = uint32(len(packNames))
					packIndexes[packName] = packIndex
					packNames = append(packNames, packName)
				}
				encodingByte := byte(0)
				if encoding == EncodingRaw {
					encodingByte = 1
				}
				if _, err := blocksFile.Write(encodeBlockV3(blockV3Record{PackIndex: packIndex, PackOffset: packOffset, StoredBytes: int64(len(stored)), RawOffset: rawOffset, RawBytes: int64(n), Digest: blockHash, Encoding: encodingByte})); err != nil {
					_ = stream.Close()
					return BuildResult{}, err
				}
				rawOffset += int64(n)
				result.BlockCount++
				if objectBlocks == ^uint32(0) {
					_ = stream.Close()
					return BuildResult{}, fmt.Errorf("object %s exceeds pack v3 block count", ref.SHA256)
				}
				objectBlocks++
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
		digest, err := decodeDigest(ref.SHA256)
		if err != nil {
			return BuildResult{}, err
		}
		if _, err := objectsFile.Write(encodeObjectV3(objectV3Record{Digest: digest, RawBytes: ref.RawBytes, FirstBlock: firstBlock, BlockCount: objectBlocks})); err != nil {
			return BuildResult{}, err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return BuildResult{}, err
	}
	if err := rows.Close(); err != nil {
		return BuildResult{}, err
	}
	if err := writer.close(); err != nil {
		return BuildResult{}, err
	}
	if err := syncAndCloseIndexFiles(objectsFile, blocksFile); err != nil {
		return BuildResult{}, err
	}
	result.PackCount = writer.sequence
	meta := indexV3Meta{Version: indexV3Version, Kind: indexV3Kind, Generation: generation, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), BlockBytes: options.BlockBytes, ObjectCount: result.ObjectCount, BlockCount: result.BlockCount, Packs: packNames}
	if err := writeIndexV3Meta(temporaryDir, meta); err != nil {
		return BuildResult{}, err
	}
	if err := verifyGeneration(ctx, temporaryDir); err != nil {
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

func estimatedGenerationBytes(rawBytes int64) (int64, error) {
	const fixedOverhead = int64(1 << 20)
	if rawBytes < 0 {
		return 0, errors.New("pack generation byte estimate overflow")
	}
	compressionOverhead := rawBytes/16 + fixedOverhead
	if rawBytes > math.MaxInt64-compressionOverhead {
		return 0, errors.New("pack generation byte estimate overflow")
	}
	return rawBytes + compressionOverhead, nil
}

func buildReferenceIndex(ctx context.Context, storeDir string, path string) (*sql.DB, int64, int64, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, 0, 0, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = database.Close()
		}
	}()
	if _, err := database.ExecContext(ctx, `pragma journal_mode=off; pragma synchronous=off; pragma temp_store=file; pragma cache_size=-16384; create table pack_references (digest text primary key, raw_bytes integer not null) without rowid`); err != nil {
		return nil, 0, 0, err
	}
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	statement, err := transaction.PrepareContext(ctx, `insert into pack_references(digest, raw_bytes) values (?, ?) on conflict(digest) do update set raw_bytes = case when raw_bytes = excluded.raw_bytes then raw_bytes else -1 end`)
	if err != nil {
		_ = transaction.Rollback()
		return nil, 0, 0, err
	}
	err = filepath.WalkDir(filepath.Join(storeDir, "manifests"), func(path string, entry os.DirEntry, walkErr error) error {
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
			if _, err := statement.ExecContext(ctx, part.Object.SHA256, part.Object.RawBytes); err != nil {
				return err
			}
		}
		return nil
	})
	closeErr := statement.Close()
	if err != nil {
		_ = transaction.Rollback()
		return nil, 0, 0, fmt.Errorf("read manifests for pack build: %w", err)
	}
	if closeErr != nil {
		_ = transaction.Rollback()
		return nil, 0, 0, closeErr
	}
	if err := transaction.Commit(); err != nil {
		return nil, 0, 0, err
	}
	var conflicts int64
	if err := database.QueryRowContext(ctx, `select count(*) from pack_references where raw_bytes < 0`).Scan(&conflicts); err != nil {
		return nil, 0, 0, err
	}
	if conflicts != 0 {
		return nil, 0, 0, fmt.Errorf("%d object digest(s) have conflicting raw lengths", conflicts)
	}
	var count, bytes int64
	if err := database.QueryRowContext(ctx, `select count(*), coalesce(sum(raw_bytes), 0) from pack_references`).Scan(&count, &bytes); err != nil {
		return nil, 0, 0, err
	}
	keep = true
	return database, count, bytes, nil
}

func syncAndCloseIndexFiles(files ...*os.File) error {
	for _, file := range files {
		if err := file.Sync(); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func writeIndexV3Meta(directory string, meta indexV3Meta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(filepath.Join(directory, indexV3MetaFilename), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDirectory(directory)
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
