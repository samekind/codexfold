package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type ReadHandle struct {
	session    *Session
	generation uint64
	base       *View
	baseBytes  int64
	file       *os.File
	backing    bool
	deltaBytes int64
	size       int64
	closeOnce  sync.Once
	closeErr   error
}

func (h *ReadHandle) Size() int64 { return h.size }

func (h *ReadHandle) ReadAt(ctx context.Context, destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative session read offset")
	}
	if len(destination) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset >= h.size {
		return 0, io.EOF
	}
	if h.backing {
		limit := len(destination)
		if remaining := h.size - offset; int64(limit) > remaining {
			limit = int(remaining)
		}
		n, err := h.file.ReadAt(destination[:limit], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, err
		}
		if n < len(destination) {
			return n, io.EOF
		}
		return n, nil
	}
	written := 0
	if offset < h.baseBytes {
		need := len(destination)
		if remaining := h.baseBytes - offset; int64(need) > remaining {
			need = int(remaining)
		}
		n, err := h.base.ReadAt(ctx, destination[:need], offset)
		written += n
		offset += int64(n)
		if n != need {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return written, err
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return written, err
		}
	}
	if written < len(destination) && offset >= h.baseBytes && offset < h.size {
		deltaOffset := offset - h.baseBytes
		need := len(destination) - written
		if remaining := h.deltaBytes - deltaOffset; int64(need) > remaining {
			need = int(remaining)
		}
		n, err := h.file.ReadAt(destination[written:written+need], deltaOffset)
		written += n
		if n != need {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return written, err
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return written, err
		}
	}
	if written < len(destination) {
		return written, io.EOF
	}
	return written, nil
}

func (h *ReadHandle) Close() error {
	h.closeOnce.Do(func() {
		h.closeErr = errors.Join(h.file.Close(), h.session.releaseReader(h.generation))
	})
	return h.closeErr
}

type WriteHandle struct {
	session         *Session
	leasePath       string
	lease           *os.File
	mu              sync.Mutex
	closed          bool
	dirty           bool
	appendHasher    hash.Hash
	appendHashPath  string
	appendHashBytes int64
	appendHashValid bool
}

func (h *WriteHandle) Append(ctx context.Context, data []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, errors.New("writer is closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	state := h.session.State()
	path := state.DeltaPath
	if state.BackingPath != "" {
		path = state.BackingPath
	}
	if err := h.ensureAppendHash(path); err != nil {
		return 0, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return 0, fmt.Errorf("open append target: %w", err)
	}
	n, writeErr := file.Write(data)
	closeErr := file.Close()
	if n > 0 {
		_, _ = h.appendHasher.Write(data[:n])
		h.appendHashBytes += int64(n)
		h.dirty = true
	}
	if writeErr != nil {
		return n, writeErr
	}
	if closeErr != nil {
		return n, closeErr
	}
	return n, nil
}

func (h *WriteHandle) WriteAt(ctx context.Context, data []byte, offset int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, errors.New("writer is closed")
	}
	if offset < 0 {
		return 0, errors.New("negative write offset")
	}
	path, err := h.session.ensureBacking(ctx)
	if err != nil {
		return 0, err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	n, writeErr := file.WriteAt(data, offset)
	closeErr := file.Close()
	if n > 0 {
		h.dirty = true
		h.appendHashValid = false
	}
	if writeErr != nil {
		return n, writeErr
	}
	return n, closeErr
}

func (h *WriteHandle) Truncate(ctx context.Context, size int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("writer is closed")
	}
	if size < 0 {
		return errors.New("negative truncate size")
	}
	visible, err := h.session.VisibleInfo()
	if err != nil {
		return err
	}
	if size == visible.Size {
		return nil
	}
	path, err := h.session.ensureBacking(ctx)
	if err != nil {
		return err
	}
	if err := os.Truncate(path, size); err != nil {
		return err
	}
	h.dirty = true
	h.appendHashValid = false
	return nil
}

func (h *WriteHandle) Sync() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.syncAndCheckpointLocked()
}

func (h *WriteHandle) syncAndCheckpointLocked() error {
	if h.closed {
		return errors.New("writer is closed")
	}
	state := h.session.State()
	path := state.DeltaPath
	if state.BackingPath != "" {
		path = state.BackingPath
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	if !h.dirty {
		return nil
	}
	var activeIdentity *sessionStateFileIdentity
	if h.appendHashValid && h.appendHashPath == filepath.Clean(path) {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != h.appendHashBytes {
			h.appendHashValid = false
		} else {
			identity := sessionStateFileIdentity{Path: filepath.Clean(path), Bytes: h.appendHashBytes, SHA256: hex.EncodeToString(h.appendHasher.Sum(nil))}
			activeIdentity = &identity
		}
	}
	if err := refreshSessionStateCheckpointWithActiveIdentity(h.session.statePath, state, activeIdentity); err != nil {
		return err
	}
	h.dirty = false
	return nil
}

func (h *WriteHandle) ensureAppendHash(path string) error {
	path = filepath.Clean(path)
	if h.appendHashValid && h.appendHashPath == path {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if stateCheckpointBytesHashed != nil {
		stateCheckpointBytesHashed(bytesRead)
	}
	h.appendHasher = hasher
	h.appendHashPath = path
	h.appendHashBytes = bytesRead
	h.appendHashValid = true
	return nil
}

func (h *WriteHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	var checkpointErr error
	if h.dirty {
		checkpointErr = h.syncAndCheckpointLocked()
	}
	h.closed = true
	unlockErr := unlockWriterFile(h.lease)
	closeErr := h.lease.Close()
	h.session.mu.Lock()
	h.session.writerOpen = false
	h.session.mu.Unlock()
	return errors.Join(checkpointErr, unlockErr, closeErr)
}
