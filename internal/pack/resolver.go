package pack

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
	"sort"
	"strings"
	"sync"

	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/storage"
	"github.com/klauspost/compress/zstd"
)

type OpenOptions struct {
	CacheBytes    int64
	BypassOSCache bool
}

type Resolver struct {
	directory            string
	index                Index
	objects              map[string]Object
	packs                map[string]*os.File
	cache                *blockCache
	lease                *storage.Lease
	bypassOSCacheApplied bool
	closeOnce            sync.Once
}

func Open(storeDir string, options OpenOptions) (*Resolver, error) {
	current, err := os.ReadFile(filepath.Join(storeDir, "packs", "CURRENT"))
	if err != nil {
		return nil, fmt.Errorf("read pack CURRENT: %w", err)
	}
	generation := strings.TrimSpace(string(current))
	if !safeGeneration(generation) {
		return nil, fmt.Errorf("unsafe pack generation %q", generation)
	}
	if options.CacheBytes == 0 {
		options.CacheBytes = defaultCacheBytes
	}
	return openGeneration(filepath.Join(storeDir, "packs", generation), options.CacheBytes, options.BypassOSCache)
}

func openGeneration(directory string, cacheBytes int64, bypassOSCache bool) (*Resolver, error) {
	lease, err := storage.AcquireLease(filepath.Join(directory, "leases"), "resolver")
	if err != nil {
		return nil, fmt.Errorf("acquire pack generation lease: %w", err)
	}
	keepLease := false
	defer func() {
		if !keepLease {
			_ = lease.Close()
		}
	}()
	data, err := os.ReadFile(filepath.Join(directory, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("read pack index: %w", err)
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decode pack index: %w", err)
	}
	directoryName := filepath.Base(directory)
	if directoryName != index.Generation && !strings.HasPrefix(directoryName, ".generation-") {
		return nil, fmt.Errorf("pack index generation %q does not match directory %q", index.Generation, filepath.Base(directory))
	}
	if err := validateIndex(index); err != nil {
		return nil, err
	}
	resolver := &Resolver{directory: directory, index: index, objects: make(map[string]Object, len(index.Objects)), packs: make(map[string]*os.File), cache: newBlockCache(cacheBytes), lease: lease}
	for _, object := range index.Objects {
		resolver.objects[object.SHA256] = object
		for _, block := range object.Blocks {
			if _, ok := resolver.packs[block.Pack]; ok {
				continue
			}
			file, err := os.Open(filepath.Join(directory, block.Pack))
			if err != nil {
				_ = resolver.Close()
				return nil, fmt.Errorf("open pack %s: %w", block.Pack, err)
			}
			if bypassOSCache {
				applied, err := configureNoCache(file)
				if err != nil {
					_ = file.Close()
					_ = resolver.Close()
					return nil, fmt.Errorf("disable OS cache for pack %s: %w", block.Pack, err)
				}
				resolver.bypassOSCacheApplied = resolver.bypassOSCacheApplied || applied
			}
			resolver.packs[block.Pack] = file
		}
	}
	for _, object := range index.Objects {
		for blockIndex, block := range object.Blocks {
			info, err := resolver.packs[block.Pack].Stat()
			if err != nil {
				_ = resolver.Close()
				return nil, fmt.Errorf("stat pack %s: %w", block.Pack, err)
			}
			if block.PackOffset > info.Size() || block.StoredBytes > info.Size()-block.PackOffset {
				_ = resolver.Close()
				return nil, fmt.Errorf("packed block %s:%d exceeds %s size", object.SHA256, blockIndex, block.Pack)
			}
		}
	}
	keepLease = true
	return resolver, nil
}

func (r *Resolver) OSCacheBypassApplied() bool { return r.bypassOSCacheApplied }

func (r *Resolver) ReadAt(ctx context.Context, ref fold.ObjectRef, destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative object read offset")
	}
	if len(destination) == 0 {
		return 0, nil
	}
	object, ok := r.objects[ref.SHA256]
	if !ok {
		return 0, fmt.Errorf("object %s is not packed", ref.SHA256)
	}
	if object.RawBytes != ref.RawBytes {
		return 0, fmt.Errorf("object %s raw size %d, want %d", ref.SHA256, object.RawBytes, ref.RawBytes)
	}
	if offset >= object.RawBytes {
		return 0, io.EOF
	}
	written := 0
	for written < len(destination) && offset < object.RawBytes {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		blockIndex := sort.Search(len(object.Blocks), func(index int) bool {
			block := object.Blocks[index]
			return block.RawOffset+block.RawBytes > offset
		})
		if blockIndex == len(object.Blocks) {
			return written, fmt.Errorf("object %s has no block for offset %d", object.SHA256, offset)
		}
		block := object.Blocks[blockIndex]
		data, err := r.readBlock(object.SHA256, blockIndex, block)
		if err != nil {
			return written, err
		}
		inside := offset - block.RawOffset
		copied := copy(destination[written:], data[inside:])
		written += copied
		offset += int64(copied)
	}
	if written < len(destination) {
		return written, io.EOF
	}
	return written, nil
}

func (r *Resolver) readBlock(objectDigest string, blockIndex int, block Block) ([]byte, error) {
	key := fmt.Sprintf("%s:%d", objectDigest, blockIndex)
	if data, ok := r.cache.get(key); ok {
		return data, nil
	}
	file := r.packs[block.Pack]
	if file == nil {
		return nil, fmt.Errorf("pack %s is not open", block.Pack)
	}
	compressed := make([]byte, int(block.StoredBytes))
	if _, err := file.ReadAt(compressed, block.PackOffset); err != nil {
		return nil, fmt.Errorf("read packed block %s:%d: %w", objectDigest, blockIndex, err)
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("create pack decoder: %w", err)
	}
	data, err := decoder.DecodeAll(compressed, nil)
	decoder.Close()
	if err != nil {
		return nil, fmt.Errorf("decode packed block %s:%d: %w", objectDigest, blockIndex, err)
	}
	if int64(len(data)) != block.RawBytes {
		return nil, fmt.Errorf("packed block %s:%d raw size %d, want %d", objectDigest, blockIndex, len(data), block.RawBytes)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != block.SHA256 {
		return nil, fmt.Errorf("packed block %s:%d SHA-256 mismatch", objectDigest, blockIndex)
	}
	r.cache.put(key, data)
	return data, nil
}

func (r *Resolver) Close() error {
	var closeErr error
	r.closeOnce.Do(func() {
		names := make([]string, 0, len(r.packs))
		for name := range r.packs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := r.packs[name].Close(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
		if err := r.lease.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	})
	return closeErr
}
