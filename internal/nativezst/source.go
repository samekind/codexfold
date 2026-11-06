package nativezst

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samekind/codexfold/internal/codexzstd"
	"github.com/samekind/codexfold/internal/fold"
)

// Pack is one opened, published pack generation. Opening it must hold the
// generation's lease until Close.
type Pack interface {
	fold.ObjectReader
	Generation() string
	Close() error
}

// PackOpener reaches the store's packs. It is injected so the mount core does
// not depend on the pack builder and its database.
type PackOpener interface {
	// CurrentGeneration fails with os.ErrNotExist before the first pack.
	CurrentGeneration(store string) (string, error)
	Open(store string) (Pack, error)
}

// Source serves folded .zst content from the store. It shares one pack
// resolver per published generation; a resolver holds its generation's lease,
// so a reader that opened against an old generation keeps it alive.
type Source struct {
	store   string
	packs   PackOpener
	loose   *fold.ObjectStore
	mu      sync.Mutex
	current *sharedResolver
	streams map[string]*sharedStream
	closed  bool
}

type sharedResolver struct {
	resolver   Pack
	generation string
	references int
	retired    bool
}

// NewSource serves from loose objects only when packs is nil.
func NewSource(store string, packs PackOpener) *Source {
	return &Source{store: store, packs: packs, loose: fold.NewObjectStore(store)}
}

func (s *Source) Store() string { return s.store }

// acquire returns the resolver for the current generation, or nil before the
// first pack exists. Callers release it exactly once.
func (s *Source) acquire() (*sharedResolver, error) {
	if s.packs == nil {
		return nil, nil
	}
	generation, err := s.packs.CurrentGeneration(s.store)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("native .zst source is closed")
	}
	if s.current != nil && s.current.generation == generation {
		s.current.references++
		return s.current, nil
	}
	resolver, err := s.packs.Open(s.store)
	if err != nil {
		return nil, err
	}
	if s.current != nil {
		s.current.retired = true
		if s.current.references == 0 {
			_ = s.current.resolver.Close()
		}
	}
	s.current = &sharedResolver{resolver: resolver, generation: resolver.Generation(), references: 1}
	return s.current, nil
}

func (s *Source) release(shared *sharedResolver) {
	if shared == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	shared.references--
	if shared.references == 0 && (shared.retired || s.closed) {
		_ = shared.resolver.Close()
		if s.current == shared {
			s.current = nil
		}
	}
}

func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for key, stream := range s.streams {
		if stream.expiry != nil {
			stream.expiry.Stop()
		}
		delete(s.streams, key)
		go stream.shutdown()
	}
	if s.current != nil && s.current.references == 0 {
		err := s.current.resolver.Close()
		s.current = nil
		return err
	}
	return nil
}

// ExistingObjects lets a fold reuse objects the current pack already holds.
func (s *Source) ExistingObjects() (fold.ObjectReader, func(), error) {
	shared, err := s.acquire()
	if err != nil {
		return nil, nil, err
	}
	if shared == nil {
		return nil, func() {}, nil
	}
	return shared.resolver, func() { s.release(shared) }, nil
}

// openObject prefers loose objects, then the shared resolver, and retries
// once against a newer generation: a loose object may have been packed and
// retired after this reader's resolver was opened.
func (s *Source) openObject(ctx context.Context, ref fold.ObjectRef, shared **sharedResolver) (io.ReadCloser, error) {
	if s.loose.HasObject(ref) {
		if stream, err := s.loose.OpenObject(ctx, ref); err == nil {
			return stream, nil
		}
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if *shared != nil {
			stream, err := (*shared).resolver.OpenObject(ctx, ref)
			if err == nil {
				return stream, nil
			}
			lastErr = err
		}
		if attempt == 0 {
			next, err := s.acquire()
			if err != nil {
				return nil, err
			}
			if next == *shared {
				s.release(next)
				break
			}
			s.release(*shared)
			*shared = next
			if s.loose.HasObject(ref) {
				if stream, err := s.loose.OpenObject(ctx, ref); err == nil {
					return stream, nil
				}
			}
		}
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return nil, fmt.Errorf("open folded object %s: %w", ref.SHA256, lastErr)
}

func (s *Source) loadParts(marker Marker) ([]fold.Part, error) {
	if err := marker.Validate(); err != nil {
		return nil, err
	}
	if !codexzstd.Supported() {
		return nil, codexzstd.ErrUnsupported
	}
	manifestPath, err := ManifestPath(s.store, marker)
	if err != nil {
		return nil, err
	}
	manifest, err := fold.LoadManifestPath(manifestPath)
	if err != nil {
		return nil, err
	}
	if manifest.Source.Bytes != marker.ContentBytes || manifest.Source.SHA256 != marker.ContentSHA256 {
		return nil, fmt.Errorf("native .zst manifest %s does not match its marker", marker.Manifest)
	}
	return manifest.Parts, nil
}

// Stream re-encodes the file once from the start, for a caller that reads it
// sequentially (a proof, a restore). It spills nothing to disk.
func (s *Source) Stream(ctx context.Context, marker Marker) (io.ReadCloser, error) {
	parts, err := s.loadParts(marker)
	if err != nil {
		return nil, err
	}
	return s.encode(ctx, marker, parts), nil
}

func (s *Source) encode(ctx context.Context, marker Marker, parts []fold.Part) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	output, input := io.Pipe()
	go func() {
		content := &contentReader{ctx: ctx, source: s, parts: parts}
		err := codexzstd.Encode(ctx, input, content, marker.ContentBytes)
		content.close()
		input.CloseWithError(err)
	}()
	return &pipeStream{PipeReader: output, cancel: cancel}
}

type pipeStream struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (p *pipeStream) Close() error {
	p.cancel()
	return p.PipeReader.Close()
}

// Open returns a random-access reader of the exact original .zst bytes. All
// readers of one file share one encoder: zstd output can only be produced
// from the start of the frame, and the FSKit extension reads a file through
// several handles at once, out of order, so a stream per handle re-encoded
// the file from the beginning over and over.
func (s *Source) Open(marker Marker) (*Reader, error) {
	key, err := ManifestKey(marker.Manifest)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.streams == nil {
		s.streams = make(map[string]*sharedStream)
	}
	stream := s.streams[key]
	if stream != nil && stream.marker == marker && !stream.failed() {
		stream.references++
		if stream.expiry != nil {
			stream.expiry.Stop()
			stream.expiry = nil
		}
		s.mu.Unlock()
		return &Reader{stream: stream, marker: marker}, nil
	}
	s.mu.Unlock()
	parts, err := s.loadParts(marker)
	if err != nil {
		return nil, err
	}
	stream, err = s.newSharedStream(key, marker, parts)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if current := s.streams[key]; current != nil && current.marker == marker && !current.failed() {
		current.references++
		if current.expiry != nil {
			current.expiry.Stop()
			current.expiry = nil
		}
		s.mu.Unlock()
		stream.shutdown()
		return &Reader{stream: current, marker: marker}, nil
	}
	if previous := s.streams[key]; previous != nil && previous.references == 0 {
		previous.shutdown()
	}
	stream.references = 1
	s.streams[key] = stream
	s.mu.Unlock()
	return &Reader{stream: stream, marker: marker}, nil
}

const (
	// A read asks the encoder for this much beyond what it needs, so the
	// extension's next prefetch block is usually ready.
	streamReadAhead = 8 << 20
	streamChunk     = 256 << 10
	// An unused stream that got this far is kept a while for a reopen; one
	// that only produced a file's head is cheaper to redo than to keep.
	streamKeepBytes = 4 << 20
	streamKeepFor   = 2 * time.Minute
)

// streamsStarted counts shared encoders, for tests.
var streamsStarted atomic.Int64

// sharedStream is one encoder for one folded file, whose output is spilled
// to an unlinked temporary file as it is produced, so every byte is encoded
// once whatever order and handles the reads come through.
type sharedStream struct {
	source     *Source
	key        string
	marker     Marker
	spill      *os.File
	encoded    io.ReadCloser
	mu         sync.Mutex
	advanced   *sync.Cond
	frontier   int64
	target     int64
	err        error
	closed     bool
	references int
	expiry     *time.Timer
}

func (s *Source) newSharedStream(key string, marker Marker, parts []fold.Part) (*sharedStream, error) {
	directory := filepath.Join(StateRoot(s.store), "spill")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	spill, err := os.CreateTemp(directory, ".spill-*.tmp")
	if err != nil {
		return nil, err
	}
	// Unlinked at once: the space returns when the stream closes or the
	// daemon exits, and nothing is left behind for a crash to strand.
	_ = os.Remove(spill.Name())
	streamsStarted.Add(1)
	stream := &sharedStream{source: s, key: key, marker: marker, spill: spill, encoded: s.encode(context.Background(), marker, parts)}
	stream.advanced = sync.NewCond(&stream.mu)
	go stream.produce()
	return stream, nil
}

func (t *sharedStream) failed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err != nil || t.closed
}

func (t *sharedStream) produce() {
	buffer := make([]byte, streamChunk)
	for {
		t.mu.Lock()
		for !t.closed && t.err == nil && t.frontier >= t.target && t.frontier < t.marker.EncodedBytes {
			t.advanced.Wait()
		}
		if t.closed || t.err != nil || t.frontier >= t.marker.EncodedBytes {
			t.mu.Unlock()
			return
		}
		offset := t.frontier
		t.mu.Unlock()

		n, readErr := t.encoded.Read(buffer)
		var writeErr error
		if n > 0 {
			_, writeErr = t.spill.WriteAt(buffer[:n], offset)
		}
		t.mu.Lock()
		if writeErr == nil {
			t.frontier += int64(n)
		}
		switch {
		case writeErr != nil:
			t.err = writeErr
		case errors.Is(readErr, io.EOF):
			if t.frontier != t.marker.EncodedBytes {
				t.err = fmt.Errorf("folded .zst ended at %d of %d bytes", t.frontier, t.marker.EncodedBytes)
			}
		case readErr != nil:
			t.err = readErr
		}
		t.advanced.Broadcast()
		t.mu.Unlock()
	}
}

// await returns once [0, end) is in the spill file.
func (t *sharedStream) await(end int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if want := min(end+streamReadAhead, t.marker.EncodedBytes); want > t.target {
		t.target = want
		t.advanced.Broadcast()
	}
	for t.frontier < end && t.err == nil && !t.closed {
		t.advanced.Wait()
	}
	if t.frontier >= end {
		return nil
	}
	if t.err != nil {
		return t.err
	}
	return os.ErrClosed
}

func (t *sharedStream) shutdown() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.advanced.Broadcast()
	t.mu.Unlock()
	_ = t.encoded.Close()
	_ = t.spill.Close()
}

// release drops one reader. A stream that produced little is closed at
// once; a large one stays briefly for a reopen.
func (t *sharedStream) release() {
	s := t.source
	s.mu.Lock()
	defer s.mu.Unlock()
	t.references--
	if t.references > 0 {
		return
	}
	t.mu.Lock()
	frontier := t.frontier
	t.mu.Unlock()
	if frontier < streamKeepBytes || s.closed || t.failed() {
		if s.streams[t.key] == t {
			delete(s.streams, t.key)
		}
		go t.shutdown()
		return
	}
	t.expiry = time.AfterFunc(streamKeepFor, func() {
		s.mu.Lock()
		if t.references != 0 {
			s.mu.Unlock()
			return
		}
		if s.streams[t.key] == t {
			delete(s.streams, t.key)
		}
		s.mu.Unlock()
		t.shutdown()
	})
}

// Reader serves one open handle of a folded file.
type Reader struct {
	stream *sharedStream
	marker Marker
	mu     sync.Mutex
	closed bool
}

func (r *Reader) Size() int64 { return r.marker.EncodedBytes }

func (r *Reader) Marker() Marker { return r.marker }

func (r *Reader) ReadAt(destination []byte, offset int64) (int, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return 0, os.ErrClosed
	}
	if offset < 0 {
		return 0, errors.New("negative read offset")
	}
	size := r.marker.EncodedBytes
	if offset >= size {
		return 0, io.EOF
	}
	end := min(offset+int64(len(destination)), size)
	if err := r.stream.await(end); err != nil {
		return 0, err
	}
	n, err := r.stream.spill.ReadAt(destination[:end-offset], offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}

func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.stream.release()
	return nil
}

// contentReader concatenates a manifest's parts: decoded rollout content. It
// runs on the encoder goroutine and owns its resolver reference.
type contentReader struct {
	ctx     context.Context
	source  *Source
	parts   []fold.Part
	shared  *sharedResolver
	opened  bool
	index   int
	current io.ReadCloser
}

func (c *contentReader) close() {
	if c.current != nil {
		_ = c.current.Close()
		c.current = nil
	}
	c.source.release(c.shared)
	c.shared = nil
}

func (c *contentReader) Read(destination []byte) (int, error) {
	for {
		if c.current == nil {
			if c.index >= len(c.parts) {
				return 0, io.EOF
			}
			if !c.opened {
				shared, err := c.source.acquire()
				if err != nil {
					return 0, err
				}
				c.shared, c.opened = shared, true
			}
			stream, err := c.source.openObject(c.ctx, c.parts[c.index].Object, &c.shared)
			if err != nil {
				return 0, err
			}
			c.current = stream
			c.index++
		}
		n, err := c.current.Read(destination)
		if errors.Is(err, io.EOF) {
			closeErr := c.current.Close()
			c.current = nil
			if closeErr != nil {
				return n, closeErr
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}
