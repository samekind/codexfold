package fold

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/jstar0/codexfold/internal/cdc"
	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/jsonraw"
)

type FoldOptions struct {
	StoreDir         string
	Apply            bool
	Overwrite        bool
	RemoveSource     bool
	AllowActive      bool
	FieldThreshold   int64
	MaxJSONLineBytes int64
	CDC              cdc.Options
	beforeCommit     func() error
}

type FoldResult struct {
	SessionID        string `json:"session_id"`
	SourcePath       string `json:"source_path"`
	ManifestPath     string `json:"manifest_path"`
	SourceBytes      int64  `json:"source_bytes"`
	SourceSHA256     string `json:"source_sha256"`
	PartCount        int    `json:"part_count"`
	FieldParts       int    `json:"field_parts"`
	ResidualParts    int    `json:"residual_parts"`
	UniqueObjects    int    `json:"unique_objects"`
	ReusedObjects    int    `json:"reused_objects"`
	NewStoredBytes   int64  `json:"new_stored_bytes"`
	OversizedLines   int64  `json:"oversized_lines"`
	InvalidJSONLines int64  `json:"invalid_json_lines"`
	Verified         bool   `json:"verified"`
	DryRun           bool   `json:"dry_run"`
	RemovedSource    bool   `json:"removed_source"`
}

func Fold(ctx context.Context, session codex.Session, options FoldOptions) (FoldResult, error) {
	if options.StoreDir == "" {
		return FoldResult{}, errors.New("fold store directory is required")
	}
	if options.RemoveSource && !options.Apply {
		return FoldResult{}, errors.New("--remove-source requires --apply")
	}
	if options.RemoveSource && !session.Archived && !options.AllowActive {
		return FoldResult{}, errors.New("refusing to remove a non-archived rollout without --allow-active")
	}
	if options.FieldThreshold <= 0 {
		options.FieldThreshold = 4 * 1024
	}
	if options.MaxJSONLineBytes <= 0 {
		options.MaxJSONLineBytes = 32 * 1024 * 1024
	}
	if options.CDC.MinBytes <= 0 {
		options.CDC = cdc.Options{MinBytes: 4 * 1024, AverageBytes: 16 * 1024, MaxBytes: 64 * 1024}
	}
	manifestPath := ManifestPath(options.StoreDir, session.ID)
	if options.Apply && !options.Overwrite {
		if _, err := os.Stat(manifestPath); err == nil {
			return FoldResult{}, fmt.Errorf("fold manifest already exists: %s", manifestPath)
		} else if err != nil && !os.IsNotExist(err) {
			return FoldResult{}, err
		}
	}

	file, err := os.Open(session.RolloutPath)
	if err != nil {
		return FoldResult{}, fmt.Errorf("open rollout: %w", err)
	}
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil {
		return FoldResult{}, fmt.Errorf("stat rollout: %w", err)
	}

	manifest := Manifest{
		Version:   ManifestVersion,
		Kind:      ManifestKind,
		CreatedAt: newManifestTimestamp(),
		Session: ManifestSession{
			ID: session.ID, Title: session.Title, CWD: session.CWD,
			RolloutPath: session.RolloutPath, Archived: session.Archived,
		},
		Settings: ManifestSettings{
			FieldThreshold: options.FieldThreshold, MaxJSONLineBytes: options.MaxJSONLineBytes,
			CDCMinBytes: options.CDC.MinBytes, CDCAverageBytes: options.CDC.AverageBytes,
			CDCMaxBytes: options.CDC.MaxBytes, Compression: "zstd",
		},
		Parts: make([]Part, 0),
	}
	result := FoldResult{
		SessionID: session.ID, SourcePath: session.RolloutPath,
		ManifestPath: manifestPath, DryRun: !options.Apply,
	}
	store := NewObjectStore(options.StoreDir)
	sourceHash := sha256.New()
	reconstructionHash := sha256.New()
	var reconstructionBytes int64

	appendPart := func(kind string, path string, data []byte) error {
		if len(data) == 0 {
			return nil
		}
		ref, reused, err := store.Put(data, options.Apply)
		if err != nil {
			return err
		}
		manifest.Parts = append(manifest.Parts, Part{Kind: kind, JSONPath: path, Object: ref})
		_, _ = reconstructionHash.Write(data)
		reconstructionBytes += int64(len(data))
		if kind == PartField {
			result.FieldParts++
		} else {
			result.ResidualParts++
		}
		if reused {
			result.ReusedObjects++
		} else {
			result.UniqueObjects++
			result.NewStoredBytes += ref.StoredBytes
		}
		return nil
	}
	chunker, err := cdc.New(options.CDC, func(chunk cdc.Chunk) error {
		return appendPart(PartResidual, "", chunk.Data)
	})
	if err != nil {
		return FoldResult{}, err
	}
	flushResidual := func() error { return chunker.Finish() }
	processLine := func(line []byte) error {
		spans, err := jsonraw.FindStringSpans(line, options.FieldThreshold)
		if err != nil {
			result.InvalidJSONLines++
			return chunker.Write(line)
		}
		cursor := 0
		for _, span := range spans {
			if err := chunker.Write(line[cursor:span.Start]); err != nil {
				return err
			}
			if err := flushResidual(); err != nil {
				return err
			}
			if err := appendPart(PartField, span.Path, line[span.Start:span.End]); err != nil {
				return err
			}
			cursor = span.End
		}
		return chunker.Write(line[cursor:])
	}

	reader := bufio.NewReaderSize(io.LimitReader(file, before.Size()), 1024*1024)
	var line bytes.Buffer
	oversized := false
	for {
		if err := ctx.Err(); err != nil {
			return FoldResult{}, err
		}
		fragment, readErr := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			_, _ = sourceHash.Write(fragment)
			result.SourceBytes += int64(len(fragment))
			if oversized {
				if err := chunker.Write(fragment); err != nil {
					return FoldResult{}, err
				}
			} else if int64(line.Len()+len(fragment)) > options.MaxJSONLineBytes {
				oversized = true
				result.OversizedLines++
				if err := chunker.Write(line.Bytes()); err != nil {
					return FoldResult{}, err
				}
				line.Reset()
				if err := chunker.Write(fragment); err != nil {
					return FoldResult{}, err
				}
			} else {
				line.Write(fragment)
			}
		}
		switch {
		case readErr == nil:
			if !oversized {
				if err := processLine(line.Bytes()); err != nil {
					return FoldResult{}, err
				}
			}
			line.Reset()
			oversized = false
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			if !oversized && line.Len() > 0 {
				if err := processLine(line.Bytes()); err != nil {
					return FoldResult{}, err
				}
			}
			if err := flushResidual(); err != nil {
				return FoldResult{}, err
			}
			goto complete
		default:
			return FoldResult{}, fmt.Errorf("read rollout: %w", readErr)
		}
	}

complete:
	result.SourceSHA256 = hex.EncodeToString(sourceHash.Sum(nil))
	manifest.Source = ManifestSource{Bytes: result.SourceBytes, SHA256: result.SourceSHA256}
	result.PartCount = len(manifest.Parts)
	reconstructedSHA := hex.EncodeToString(reconstructionHash.Sum(nil))
	result.Verified = reconstructionBytes == result.SourceBytes && reconstructedSHA == result.SourceSHA256
	if !result.Verified {
		return FoldResult{}, errors.New("fold segmentation verification failed")
	}
	if options.beforeCommit != nil {
		if err := options.beforeCommit(); err != nil {
			return FoldResult{}, fmt.Errorf("before fold commit: %w", err)
		}
	}
	after, err := os.Stat(session.RolloutPath)
	if err != nil {
		return FoldResult{}, fmt.Errorf("stat rollout after fold: %w", err)
	}
	if before.Size() != after.Size() || before.ModTime().UnixNano() != after.ModTime().UnixNano() || !os.SameFile(before, after) {
		return FoldResult{}, errors.New("rollout changed during fold; manifest was not committed")
	}
	if err := verifyCurrentSource(session.RolloutPath, before, manifest.Source); err != nil {
		return FoldResult{}, err
	}
	if !options.Apply {
		return result, nil
	}
	if err := verifyStoredManifest(ctx, store, manifest); err != nil {
		return FoldResult{}, err
	}
	if err := store.SyncPending(ctx); err != nil {
		return FoldResult{}, err
	}
	if err := writeManifest(options.StoreDir, manifest, options.Overwrite); err != nil {
		return FoldResult{}, err
	}
	if options.RemoveSource {
		if err := os.Remove(session.RolloutPath); err != nil {
			return FoldResult{}, fmt.Errorf("remove verified rollout source: %w", err)
		}
		result.RemovedSource = true
	}
	return result, nil
}

func verifyCurrentSource(path string, initial os.FileInfo, source ManifestSource) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reopen rollout for commit verification: %w", err)
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("rehash rollout for commit verification: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close rollout after commit verification: %w", closeErr)
	}
	after, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat rollout after commit verification: %w", err)
	}
	if initial.Size() != after.Size() || initial.ModTime().UnixNano() != after.ModTime().UnixNano() || !os.SameFile(initial, after) {
		return errors.New("rollout changed during fold; manifest was not committed")
	}
	if err := verifySourceDigest(hasher, bytesRead, source); err != nil {
		return fmt.Errorf("rollout changed during fold; manifest was not committed: %w", err)
	}
	return nil
}

func verifyStoredManifest(ctx context.Context, store *ObjectStore, manifest Manifest) error {
	hasher := sha256.New()
	var bytesWritten int64
	for _, part := range manifest.Parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := store.Read(part.Object)
		if err != nil {
			return err
		}
		_, _ = hasher.Write(data)
		bytesWritten += int64(len(data))
	}
	return verifySourceDigest(hasher, bytesWritten, manifest.Source)
}

func verifySourceDigest(hasher hash.Hash, bytesWritten int64, source ManifestSource) error {
	if bytesWritten != source.Bytes {
		return fmt.Errorf("reconstructed bytes %d, want %d", bytesWritten, source.Bytes)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if digest != source.SHA256 {
		return fmt.Errorf("reconstructed SHA-256 %s, want %s", digest, source.SHA256)
	}
	return nil
}
