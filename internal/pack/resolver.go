package pack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

type OpenOptions struct {
	CacheBytes    int64
	BypassOSCache bool
}

type Resolver struct {
	directory            string
	index                Index
	objects              map[string]Object
	v3                   *indexV3
	generation           string
	objectCount          int64
	packs                map[string]*os.File
	cache                *blockCache
	decoders             chan *zstd.Decoder
	decoderFactory       func() (*zstd.Decoder, error)
	lease                *storage.Lease
	bypassOSCacheApplied bool
	shared               *resolverResources
	closeOnce            sync.Once
	closeErr             error
}

type resolverResourceKey struct {
	directory     string
	cacheBytes    int64
	bypassOSCache bool
}

type resolverResources struct {
	key                  resolverResourceKey
	directory            string
	index                Index
	objects              map[string]Object
	v3                   *indexV3
	generation           string
	objectCount          int64
	packs                map[string]*os.File
	cache                *blockCache
	decoders             chan *zstd.Decoder
	lease                *storage.Lease
	bypassOSCacheApplied bool
	references           int
}

var resolverRegistry = struct {
	sync.Mutex
	resources map[resolverResourceKey]*resolverResources
}{resources: make(map[resolverResourceKey]*resolverResources)}

func Open(storeDir string, options OpenOptions) (*Resolver, error) {
	generation, err := CurrentGeneration(storeDir)
	if err != nil {
		return nil, err
	}
	if options.CacheBytes == 0 {
		options.CacheBytes = defaultCacheBytes
	}
	return openGeneration(filepath.Join(storeDir, "packs", generation), options.CacheBytes, options.BypassOSCache)
}

func CurrentGeneration(storeDir string) (string, error) {
	current, err := os.ReadFile(filepath.Join(storeDir, "packs", "CURRENT"))
	if err != nil {
		return "", fmt.Errorf("read pack CURRENT: %w", err)
	}
	generation := strings.TrimSpace(string(current))
	if !safeGeneration(generation) {
		return "", fmt.Errorf("unsafe pack generation %q", generation)
	}
	return generation, nil
}

func openGeneration(directory string, cacheBytes int64, bypassOSCache bool) (*Resolver, error) {
	directory = filepath.Clean(directory)
	if cacheBytes < 0 {
		cacheBytes = 0
	}
	key := resolverResourceKey{directory: directory, cacheBytes: cacheBytes, bypassOSCache: bypassOSCache}
	resolverRegistry.Lock()
	defer resolverRegistry.Unlock()
	if shared := resolverRegistry.resources[key]; shared != nil {
		shared.references++
		return resolverFromResources(shared), nil
	}
	shared, err := loadResolverResources(key)
	if err != nil {
		return nil, err
	}
	shared.references = 1
	resolverRegistry.resources[key] = shared
	return resolverFromResources(shared), nil
}

func loadResolverResources(key resolverResourceKey) (*resolverResources, error) {
	directory := key.directory
	lease, err := storage.AcquireLease(filepath.Join(directory, "leases"), "resolver")
	if err != nil {
		return nil, fmt.Errorf("acquire pack generation lease: %w", err)
	}
	var shared *resolverResources
	keepResources := false
	defer func() {
		if keepResources {
			return
		}
		if shared != nil {
			_ = closeResolverResources(shared)
		} else {
			_ = lease.Close()
		}
	}()
	var index Index
	var v3 *indexV3
	if _, statErr := os.Stat(filepath.Join(directory, indexV3MetaFilename)); statErr == nil {
		v3, err = openIndexV3(directory)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	} else {
		data, readErr := os.ReadFile(filepath.Join(directory, "index.json"))
		if readErr != nil {
			return nil, fmt.Errorf("read pack index: %w", readErr)
		}
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, fmt.Errorf("decode pack index: %w", err)
		}
		normalizeLegacyIndex(&index)
		directoryName := filepath.Base(directory)
		if directoryName != index.Generation && !strings.HasPrefix(directoryName, ".generation-") {
			return nil, fmt.Errorf("pack index generation %q does not match directory %q", index.Generation, filepath.Base(directory))
		}
		if err := validateIndex(index); err != nil {
			return nil, err
		}
	}
	decoderPoolSize := runtime.GOMAXPROCS(0)
	if decoderPoolSize < 1 {
		decoderPoolSize = 1
	}
	if decoderPoolSize > 32 {
		decoderPoolSize = 32
	}
	shared = &resolverResources{
		key: key, directory: directory, index: index, v3: v3,
		objects: make(map[string]Object, len(index.Objects)),
		packs:   make(map[string]*os.File), cache: newBlockCache(key.cacheBytes),
		decoders: make(chan *zstd.Decoder, decoderPoolSize), lease: lease,
	}
	packNames := make([]string, 0)
	if v3 != nil {
		shared.generation = v3.meta.Generation
		shared.objectCount = v3.meta.ObjectCount
		packNames = append(packNames, v3.meta.Packs...)
	} else {
		shared.generation = index.Generation
		shared.objectCount = int64(len(index.Objects))
	}
	for _, object := range index.Objects {
		shared.objects[object.SHA256] = object
		for _, block := range object.Blocks {
			packNames = append(packNames, block.Pack)
		}
	}
	for _, packName := range packNames {
		if _, ok := shared.packs[packName]; ok {
			continue
		}
		file, err := os.Open(filepath.Join(directory, packName))
		if err != nil {
			return nil, fmt.Errorf("open pack %s: %w", packName, err)
		}
		if key.bypassOSCache {
			applied, err := configureNoCache(file)
			if err != nil {
				_ = file.Close()
				return nil, fmt.Errorf("disable OS cache for pack %s: %w", packName, err)
			}
			shared.bypassOSCacheApplied = shared.bypassOSCacheApplied || applied
		}
		shared.packs[packName] = file
	}
	packSizes, err := resolverPackSizes(shared.packs)
	if err != nil {
		return nil, err
	}
	if err := validateResolverBounds(shared, packSizes); err != nil {
		return nil, err
	}
	keepResources = true
	return shared, nil
}

func resolverFromResources(shared *resolverResources) *Resolver {
	return &Resolver{
		directory: shared.directory, index: shared.index, objects: shared.objects, v3: shared.v3,
		generation: shared.generation, objectCount: shared.objectCount,
		packs: shared.packs, cache: shared.cache, decoders: shared.decoders,
		decoderFactory: newPackDecoder, lease: shared.lease,
		bypassOSCacheApplied: shared.bypassOSCacheApplied, shared: shared,
	}
}

func validateResolverBounds(shared *resolverResources, packSizes map[string]int64) error {
	if shared.v3 == nil {
		return validatePackedBlockBounds(shared.index, packSizes)
	}
	var previous [32]byte
	for position := int64(0); position < shared.v3.meta.ObjectCount; position++ {
		record, err := shared.v3.readObjectRecord(position)
		if err != nil {
			return err
		}
		if position > 0 && bytes.Compare(previous[:], record.Digest[:]) >= 0 {
			return errors.New("pack v3 object index is not strictly sorted")
		}
		previous = record.Digest
		object, err := shared.v3.objectAt(position)
		if err != nil {
			return err
		}
		if err := validateObjectBlockBounds(object, packSizes); err != nil {
			return err
		}
	}
	return nil
}

func resolverPackSizes(packs map[string]*os.File) (map[string]int64, error) {
	sizes := make(map[string]int64, len(packs))
	for name, file := range packs {
		info, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("stat pack %s: %w", name, err)
		}
		sizes[name] = info.Size()
	}
	return sizes, nil
}

func validatePackedBlockBounds(index Index, packSizes map[string]int64) error {
	for _, object := range index.Objects {
		if err := validateObjectBlockBounds(object, packSizes); err != nil {
			return err
		}
	}
	return nil
}

func validateObjectBlockBounds(object Object, packSizes map[string]int64) error {
	for blockIndex, block := range object.Blocks {
		packSize, exists := packSizes[block.Pack]
		if !exists {
			return fmt.Errorf("packed block %s:%d references unopened pack %s", object.SHA256, blockIndex, block.Pack)
		}
		if block.PackOffset > packSize || block.StoredBytes > packSize-block.PackOffset {
			return fmt.Errorf("packed block %s:%d exceeds %s size", object.SHA256, blockIndex, block.Pack)
		}
	}
	return nil
}

func (r *Resolver) OSCacheBypassApplied() bool { return r.bypassOSCacheApplied }

func (r *Resolver) Generation() string { return r.generation }

func (r *Resolver) ObjectCount() int64 { return r.objectCount }

func (r *Resolver) objectAt(position int64) (Object, error) {
	if position < 0 || position >= r.objectCount {
		return Object{}, io.EOF
	}
	if r.v3 != nil {
		return r.v3.objectAt(position)
	}
	return r.index.Objects[position], nil
}

func (r *Resolver) HasDigest(digest string) bool {
	_, ok, err := r.lookupObject(digest)
	return err == nil && ok
}

func (r *Resolver) HasObject(ref fold.ObjectRef) bool {
	object, ok, err := r.lookupObject(ref.SHA256)
	return err == nil && ok && object.RawBytes == ref.RawBytes
}

func (r *Resolver) lookupObject(digest string) (Object, bool, error) {
	if r.v3 != nil {
		return r.v3.lookup(digest)
	}
	object, ok := r.objects[digest]
	return object, ok, nil
}

func (r *Resolver) ReadAt(ctx context.Context, ref fold.ObjectRef, destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative object read offset")
	}
	if len(destination) == 0 {
		return 0, nil
	}
	object, ok, lookupErr := r.lookupObject(ref.SHA256)
	if lookupErr != nil {
		return 0, lookupErr
	}
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

type objectStream struct {
	ctx      context.Context
	resolver *Resolver
	ref      fold.ObjectRef
	offset   int64
}

func (r *Resolver) OpenObject(ctx context.Context, ref fold.ObjectRef) (io.ReadCloser, error) {
	object, ok, lookupErr := r.lookupObject(ref.SHA256)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if !ok {
		return nil, fmt.Errorf("object %s is not packed", ref.SHA256)
	}
	if object.RawBytes != ref.RawBytes {
		return nil, fmt.Errorf("object %s raw size %d, want %d", ref.SHA256, object.RawBytes, ref.RawBytes)
	}
	return &objectStream{ctx: ctx, resolver: r, ref: ref}, nil
}

func (s *objectStream) Read(destination []byte) (int, error) {
	n, err := s.resolver.ReadAt(s.ctx, s.ref, destination, s.offset)
	s.offset += int64(n)
	return n, err
}

func (s *objectStream) Close() error { return nil }

func (r *Resolver) readBlock(objectDigest string, blockIndex int, block Block) ([]byte, error) {
	key := fmt.Sprintf("%s:%d", objectDigest, blockIndex)
	if data, ok := r.cache.get(key); ok {
		return data, nil
	}
	file := r.packs[block.Pack]
	if file == nil {
		return nil, fmt.Errorf("pack %s is not open", block.Pack)
	}
	stored := make([]byte, int(block.StoredBytes))
	if _, err := file.ReadAt(stored, block.PackOffset); err != nil {
		return nil, fmt.Errorf("read packed block %s:%d: %w", objectDigest, blockIndex, err)
	}
	var data []byte
	if block.Encoding == EncodingRaw {
		data = stored
	} else {
		if block.RawBytes > int64(^uint(0)>>1) {
			return nil, fmt.Errorf("packed block %s:%d raw size exceeds platform limit", objectDigest, blockIndex)
		}
		decoder, err := r.acquireDecoder()
		if err != nil {
			return nil, fmt.Errorf("create pack decoder: %w", err)
		}
		data, err = decoder.DecodeAll(stored, make([]byte, 0, int(block.RawBytes)))
		if err != nil {
			decoder.Close()
			return nil, fmt.Errorf("decode packed block %s:%d: %w", objectDigest, blockIndex, err)
		}
		r.releaseDecoder(decoder)
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

func newPackDecoder() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
}

func (r *Resolver) acquireDecoder() (*zstd.Decoder, error) {
	select {
	case decoder := <-r.decoders:
		return decoder, nil
	default:
		return r.decoderFactory()
	}
}

func (r *Resolver) releaseDecoder(decoder *zstd.Decoder) {
	select {
	case r.decoders <- decoder:
	default:
		decoder.Close()
	}
}

func (r *Resolver) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = releaseResolverResources(r.shared)
	})
	return r.closeErr
}

func releaseResolverResources(shared *resolverResources) error {
	if shared == nil {
		return nil
	}
	resolverRegistry.Lock()
	registered := resolverRegistry.resources[shared.key]
	if registered != shared || shared.references <= 0 {
		resolverRegistry.Unlock()
		return errors.New("pack resolver resource reference is not registered")
	}
	shared.references--
	if shared.references > 0 {
		resolverRegistry.Unlock()
		return nil
	}
	delete(resolverRegistry.resources, shared.key)
	resolverRegistry.Unlock()
	return closeResolverResources(shared)
}

func closeResolverResources(shared *resolverResources) error {
	if shared == nil {
		return nil
	}
	var closeErr error
	names := make([]string, 0, len(shared.packs))
	for name := range shared.packs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := shared.packs[name].Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	if shared.v3 != nil {
		closeErr = errors.Join(closeErr, shared.v3.close())
	}
	for {
		select {
		case decoder := <-shared.decoders:
			decoder.Close()
		default:
			if shared.lease != nil {
				closeErr = errors.Join(closeErr, shared.lease.Close())
			}
			return closeErr
		}
	}
}
