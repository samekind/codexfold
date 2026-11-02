package nativezst

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

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

// Open returns a reader of the exact original .zst bytes for marker.
func (s *Source) Open(marker Marker) (*Reader, error) {
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
	return &Reader{source: s, marker: marker, parts: manifest.Parts}, nil
}

// readWindowBytes keeps recently produced bytes so that slightly reordered
// reads do not restart the encoder from the beginning of the frame.
const readWindowBytes = 32 << 20

// Reader re-encodes a folded file. zstd output can only be produced from the
// start of the frame, so a read behind the window restarts the encoder.
type Reader struct {
	source  *Source
	marker  Marker
	parts   []fold.Part
	mu      sync.Mutex
	encoded io.ReadCloser
	cancel  context.CancelFunc
	// window holds encoded bytes [windowStart, windowStart+len(window)).
	window      []byte
	windowStart int64
	closed      bool
}

func (r *Reader) Size() int64 { return r.marker.EncodedBytes }

func (r *Reader) Marker() Marker { return r.marker }

func (r *Reader) position() int64 { return r.windowStart + int64(len(r.window)) }

func (r *Reader) restart() {
	r.stopEncoder()
	ctx, cancel := context.WithCancel(context.Background())
	output, input := io.Pipe()
	go func() {
		content := &contentReader{ctx: ctx, source: r.source, parts: r.parts}
		err := codexzstd.Encode(ctx, input, content, r.marker.ContentBytes)
		content.close()
		input.CloseWithError(err)
	}()
	r.encoded = output
	r.cancel = cancel
	r.window = r.window[:0]
	r.windowStart = 0
}

func (r *Reader) stopEncoder() {
	if r.encoded != nil {
		_ = r.encoded.Close()
		r.encoded = nil
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
}

func (r *Reader) ReadAt(destination []byte, offset int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if offset < 0 {
		return 0, errors.New("negative read offset")
	}
	size := r.marker.EncodedBytes
	if offset >= size {
		return 0, io.EOF
	}
	want := int64(len(destination))
	if remaining := size - offset; want > remaining {
		want = remaining
	}
	end := offset + want
	if r.encoded == nil || offset < r.windowStart {
		r.restart()
	}
	if err := r.fill(offset, end); err != nil {
		r.stopEncoder()
		return 0, err
	}
	n := copy(destination[:want], r.window[offset-r.windowStart:end-r.windowStart])
	if int64(n) < int64(len(destination)) {
		return n, io.EOF
	}
	return n, nil
}

func (r *Reader) fill(offset int64, end int64) error {
	keep := max(int64(readWindowBytes), end-offset)
	chunk := make([]byte, 256<<10)
	for r.position() < end {
		n, err := r.encoded.Read(chunk)
		r.window = append(r.window, chunk[:n]...)
		if excess := int64(len(r.window)) - keep; excess > 0 {
			trim := min(excess, max(0, offset-r.windowStart))
			if trim > 0 {
				kept := copy(r.window, r.window[trim:])
				r.window = r.window[:kept]
				r.windowStart += trim
			}
		}
		if errors.Is(err, io.EOF) {
			if r.position() < end {
				return fmt.Errorf("folded .zst ended at %d of %d bytes", r.position(), r.marker.EncodedBytes)
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.stopEncoder()
	r.window = nil
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
