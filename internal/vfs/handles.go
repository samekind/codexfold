package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
		h.closeErr = h.file.Close()
		h.session.releaseReader(h.generation)
	})
	return h.closeErr
}

type WriteHandle struct {
	session   *Session
	leasePath string
	lease     *os.File
	mu        sync.Mutex
	closed    bool
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
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return 0, fmt.Errorf("open append target: %w", err)
	}
	n, writeErr := file.Write(data)
	closeErr := file.Close()
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
	path, err := h.session.ensureBacking(ctx)
	if err != nil {
		return err
	}
	return os.Truncate(path, size)
}

func (h *WriteHandle) Sync() error {
	h.mu.Lock()
	defer h.mu.Unlock()
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
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (h *WriteHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	unlockErr := unlockWriterFile(h.lease)
	closeErr := h.lease.Close()
	h.session.mu.Lock()
	h.session.writerOpen = false
	h.session.mu.Unlock()
	if unlockErr != nil {
		return unlockErr
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}
