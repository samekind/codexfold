package pack

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"

	"github.com/samekind/codexfold/internal/fold"
)

// estimateCompactedGeneration follows Build's encoded-block reuse decision.
// Unlike the incremental estimate, reused blocks occupy new pack bytes during
// a compaction, but they do not expand back to their much larger raw size.
// Loose or incompatible objects are conservatively priced as re-encoded raw
// data. The writer and storage guard still enforce the hard space limits.
func estimateCompactedGeneration(ctx context.Context, store string, refs *sql.DB, previous *Resolver, count, rawBytes, blockBytes int64) (int64, error) {
	rows, err := refs.QueryContext(ctx, `select digest,raw_bytes from pack_references order by digest`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	objects := fold.NewObjectStore(store)
	var reencodedRaw, copiedEncoded int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var digest string
		var size int64
		if err := rows.Scan(&digest, &size); err != nil {
			return 0, err
		}
		_, looseErr := os.Lstat(objects.ObjectPath(digest))
		if looseErr != nil && !errors.Is(looseErr, os.ErrNotExist) {
			return 0, looseErr
		}
		if looseErr == nil || previous == nil {
			if size < 0 || reencodedRaw > math.MaxInt64-size {
				return 0, errors.New("compacted generation projection overflow")
			}
			reencodedRaw += size
			continue
		}
		object, found, err := previous.lookupObject(digest)
		if err != nil {
			return 0, err
		}
		compatible := found && object.RawBytes == size
		for _, block := range object.Blocks {
			if block.RawBytes > blockBytes {
				compatible = false
			}
		}
		if !compatible {
			if size < 0 || reencodedRaw > math.MaxInt64-size {
				return 0, errors.New("compacted generation projection overflow")
			}
			reencodedRaw += size
			continue
		}
		for _, block := range object.Blocks {
			if block.StoredBytes < 0 || copiedEncoded > math.MaxInt64-block.StoredBytes {
				return 0, errors.New("compacted generation projection overflow")
			}
			copiedEncoded += block.StoredBytes
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	encodedBound, err := estimatedGenerationBytes(reencodedRaw)
	if err != nil {
		return 0, err
	}
	if copiedEncoded > math.MaxInt64-encodedBound {
		return 0, errors.New("compacted generation projection overflow")
	}
	return estimateGenerationMetadata(ctx, store, count, rawBytes, blockBytes, copiedEncoded+encodedBound)
}

// Bound newly allocated bytes rather than charging shared payloads again.
// Metadata is deliberately uncompressed and doubled to cover its index and
// recovery-archive copies; tar/catalog/header slack is counted per file.
func estimateIncrementalGeneration(ctx context.Context, store string, refs *sql.DB, previous *Resolver, count, rawBytes, blockBytes int64) (int64, error) {
	rows, err := refs.QueryContext(ctx, `select digest,raw_bytes from pack_references order by digest`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var added int64
	objects := fold.NewObjectStore(store)
	add := func(n int64) error {
		if n < 0 || added > math.MaxInt64-n {
			return errors.New("incremental projection overflow")
		}
		added += n
		return nil
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var digest string
		var size int64
		if err := rows.Scan(&digest, &size); err != nil {
			return 0, err
		}
		object, found, err := previous.lookupObject(digest)
		if err != nil {
			return 0, err
		}
		_, looseErr := os.Lstat(objects.ObjectPath(digest))
		if looseErr != nil && !errors.Is(looseErr, os.ErrNotExist) {
			return 0, looseErr
		}
		if !found || object.RawBytes != size || looseErr == nil {
			if err := add(size); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	payload, err := estimatedGenerationBytes(added)
	if err != nil {
		return 0, err
	}
	return estimateGenerationMetadata(ctx, store, count, rawBytes, blockBytes, payload)
}

func estimateGenerationMetadata(ctx context.Context, store string, count, rawBytes, blockBytes, payload int64) (int64, error) {
	added := payload
	add := func(n int64) error {
		if n < 0 || added > math.MaxInt64-n {
			return errors.New("generation metadata projection overflow")
		}
		added += n
		return nil
	}
	// At most one partial block per object, plus full blocks.
	blocks := rawBytes/blockBytes + count
	if count > math.MaxInt64/104 || blocks > math.MaxInt64/138 {
		return 0, errors.New("incremental index projection overflow")
	}
	if err := add(count * 104); err != nil {
		return 0, err
	}
	if err := add(blocks * 138); err != nil {
		return 0, err
	}
	if err := add(64 << 20); err != nil {
		return 0, err
	}
	err := filepath.WalkDir(filepath.Join(store, "manifests"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > (math.MaxInt64-4096)/2 {
			return errors.New("incremental manifest projection overflow")
		}
		return add(info.Size()*2 + 4096)
	})
	return added, err
}
