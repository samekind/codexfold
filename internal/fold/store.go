package fold

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const objectSyncWorkers = 16

type ObjectRef struct {
	SHA256      string `json:"sha256"`
	RawBytes    int64  `json:"raw_bytes"`
	StoredBytes int64  `json:"stored_bytes"`
}

type cachedObject struct {
	ref       ObjectRef
	persisted bool
}

type ObjectStore struct {
	root        string
	objects     map[string]cachedObject
	pending     map[string]struct{}
	pendingDirs map[string]struct{}
}

func NewObjectStore(root string) *ObjectStore {
	return &ObjectStore{
		root: root, objects: make(map[string]cachedObject),
		pending: make(map[string]struct{}), pendingDirs: make(map[string]struct{}),
	}
}

func (s *ObjectStore) ObjectPath(digest string) string {
	prefix := "invalid"
	if len(digest) >= 2 {
		prefix = digest[:2]
	}
	return filepath.Join(s.root, "objects", prefix, digest+".zst")
}

func (s *ObjectStore) Put(data []byte, apply bool) (ObjectRef, bool, error) {
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	if cached, ok := s.objects[digest]; ok && (!apply || cached.persisted) {
		return cached.ref, true, nil
	}
	path := s.ObjectPath(digest)
	if info, err := os.Stat(path); err == nil {
		ref := ObjectRef{SHA256: digest, RawBytes: int64(len(data)), StoredBytes: info.Size()}
		if _, err := s.Read(ref); err != nil {
			return ObjectRef{}, false, fmt.Errorf("verify existing object %s: %w", digest, err)
		}
		s.objects[digest] = cachedObject{ref: ref, persisted: true}
		return ref, true, nil
	} else if err != nil && !os.IsNotExist(err) {
		return ObjectRef{}, false, fmt.Errorf("stat object %s: %w", digest, err)
	}
	compressed, err := compressObject(data)
	if err != nil {
		return ObjectRef{}, false, err
	}
	ref := ObjectRef{SHA256: digest, RawBytes: int64(len(data)), StoredBytes: int64(len(compressed))}
	s.objects[digest] = cachedObject{ref: ref, persisted: false}
	if !apply {
		return ref, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ObjectRef{}, false, fmt.Errorf("create object directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".object-*.tmp")
	if err != nil {
		return ObjectRef{}, false, fmt.Errorf("create temporary object: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(compressed); err != nil {
		_ = temporary.Close()
		return ObjectRef{}, false, fmt.Errorf("write temporary object: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return ObjectRef{}, false, fmt.Errorf("close temporary object: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if info, statErr := os.Stat(path); statErr == nil {
			ref.StoredBytes = info.Size()
			if _, readErr := s.Read(ref); readErr != nil {
				return ObjectRef{}, false, fmt.Errorf("verify concurrently committed object %s: %w", digest, readErr)
			}
			s.objects[digest] = cachedObject{ref: ref, persisted: true}
			return ref, true, nil
		}
		return ObjectRef{}, false, fmt.Errorf("commit object: %w", err)
	}
	s.objects[digest] = cachedObject{ref: ref, persisted: true}
	s.pending[path] = struct{}{}
	s.pendingDirs[filepath.Dir(path)] = struct{}{}
	return ref, false, nil
}

func (s *ObjectStore) SyncPending(ctx context.Context) error {
	if len(s.pending) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	errorsFound := make(chan error, 1)
	var workers sync.WaitGroup
	workerCount := objectSyncWorkers
	if len(s.pending) < workerCount {
		workerCount = len(s.pending)
	}
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for path := range jobs {
				if err := syncFile(path); err != nil {
					select {
					case errorsFound <- err:
						cancel()
					default:
					}
					return
				}
			}
		}()
	}
	for path := range s.pending {
		select {
		case jobs <- path:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	select {
	case err := <-errorsFound:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for directory := range s.pendingDirs {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	if err := syncDirectory(filepath.Join(s.root, "objects")); err != nil {
		return err
	}
	clear(s.pending)
	clear(s.pendingDirs)
	return nil
}

func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open object for sync: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync object %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close synced object %s: %w", path, err)
	}
	return nil
}

func (s *ObjectStore) pendingCount() int {
	return len(s.pending)
}

func (s *ObjectStore) Read(ref ObjectRef) ([]byte, error) {
	compressed, err := os.ReadFile(s.ObjectPath(ref.SHA256))
	if err != nil {
		return nil, fmt.Errorf("read object %s: %w", ref.SHA256, err)
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	defer decoder.Close()
	data, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decode object %s: %w", ref.SHA256, err)
	}
	if int64(len(data)) != ref.RawBytes {
		return nil, fmt.Errorf("object %s raw size %d, want %d", ref.SHA256, len(data), ref.RawBytes)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return nil, fmt.Errorf("object %s SHA-256 mismatch", ref.SHA256)
	}
	return data, nil
}

func compressObject(data []byte) ([]byte, error) {
	var buffer bytes.Buffer
	encoder, err := zstd.NewWriter(&buffer, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	if _, err := encoder.Write(data); err != nil {
		encoder.Close()
		return nil, fmt.Errorf("compress object: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close zstd encoder: %w", err)
	}
	return buffer.Bytes(), nil
}
