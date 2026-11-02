// Package codexzstd reproduces the zstd frames Codex writes when it compresses
// an idle rollout, byte for byte.
//
// Codex (codex-rs/rollout/src/compression.rs) streams the rollout through
// libzstd 1.5.7 at level 3 with the source size pledged and no checksum. The
// same library and parameters produce the same bytes, so a folded rollout can
// be served back as its original .zst without keeping the compressed copy.
// Go's own zstd encoders produce different frames and cannot be used here.
package codexzstd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// Profile names the exact encoder recipe. A folded .zst records it, and is
// only ever served by a build whose Profile matches.
const Profile = "libzstd-1.5.7-level3-pledged-nochecksum"

var ErrUnsupported = errors.New("exact Codex zstd encoding is unavailable in this build")

// ErrNotReproducible means the file is a valid zstd stream that this encoder
// does not reproduce exactly, so it must stay as it is.
var ErrNotReproducible = errors.New("zstd file is not reproducible by the Codex encoder profile")

// Proof describes a .zst whose bytes were reproduced from its decoded content.
type Proof struct {
	Profile       string `json:"profile"`
	EncodedBytes  int64  `json:"encoded_bytes"`
	EncodedSHA256 string `json:"encoded_sha256"`
	ContentBytes  int64  `json:"content_bytes"`
	ContentSHA256 string `json:"content_sha256"`
}

// Encode writes the Codex frame for content, which must be exactly size bytes.
func Encode(ctx context.Context, w io.Writer, content io.Reader, size int64) error {
	encoder, err := newEncoder(w, size)
	if err != nil {
		return err
	}
	buffer := make([]byte, 128<<10)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			encoder.abort()
			return err
		}
		n, readErr := content.Read(buffer)
		if n > 0 {
			copied += int64(n)
			if copied > size {
				encoder.abort()
				return fmt.Errorf("zstd content exceeds pledged size %d", size)
			}
			if _, err := encoder.Write(buffer[:n]); err != nil {
				encoder.abort()
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			encoder.abort()
			return readErr
		}
	}
	if copied != size {
		encoder.abort()
		return fmt.Errorf("zstd content is %d bytes, pledged %d", copied, size)
	}
	return encoder.Close()
}

// NewReader streams the Codex frame for content. Closing it stops the encoder.
func NewReader(ctx context.Context, content io.Reader, size int64) io.ReadCloser {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		writer.CloseWithError(Encode(ctx, writer, content, size))
	}()
	return &pipeReader{PipeReader: reader, cancel: cancel}
}

type pipeReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *pipeReader) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}

// ContentSize reads the decoded size a single-frame zstd file declares.
func ContentSize(file io.ReaderAt) (int64, error) {
	header := make([]byte, zstd.HeaderMaxSize)
	n, err := file.ReadAt(header, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	var parsed zstd.Header
	if err := parsed.Decode(header[:n]); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNotReproducible, err)
	}
	if parsed.Skippable || !parsed.HasFCS || parsed.HasCheckSum || parsed.DictionaryID != 0 {
		return 0, ErrNotReproducible
	}
	if parsed.FrameContentSize > uint64(1<<62) {
		return 0, ErrNotReproducible
	}
	return int64(parsed.FrameContentSize), nil
}

// Verify proves that path is exactly the Codex frame of its own content. It
// decodes the file, re-encodes the content and compares every byte, without
// holding either copy in memory.
func Verify(ctx context.Context, path string) (Proof, error) {
	if !Supported() {
		return Proof{}, ErrUnsupported
	}
	file, err := os.Open(path)
	if err != nil {
		return Proof{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Proof{}, err
	}
	if !info.Mode().IsRegular() {
		return Proof{}, fmt.Errorf("%s is not a regular file", path)
	}
	size, err := ContentSize(file)
	if err != nil {
		return Proof{}, err
	}
	decoder, err := zstd.NewReader(io.NewSectionReader(file, 0, info.Size()), zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(1<<31))
	if err != nil {
		return Proof{}, err
	}
	defer decoder.Close()
	contentHash := sha256.New()
	original := &matchingWriter{source: io.NewSectionReader(file, 0, info.Size()), hash: sha256.New()}
	if err := Encode(ctx, original, io.TeeReader(decoder, contentHash), size); err != nil {
		if errors.Is(err, errMismatch) {
			return Proof{}, ErrNotReproducible
		}
		return Proof{}, fmt.Errorf("re-encode %s: %w", path, err)
	}
	if original.offset != info.Size() {
		return Proof{}, ErrNotReproducible
	}
	after, err := os.Stat(path)
	if err != nil {
		return Proof{}, err
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return Proof{}, fmt.Errorf("%s changed during verification", path)
	}
	return Proof{
		Profile: Profile, EncodedBytes: info.Size(), EncodedSHA256: hex.EncodeToString(original.hash.Sum(nil)),
		ContentBytes: size, ContentSHA256: hex.EncodeToString(contentHash.Sum(nil)),
	}, nil
}

var errMismatch = errors.New("re-encoded zstd bytes differ")

// matchingWriter compares what the encoder emits with the original file.
type matchingWriter struct {
	source *io.SectionReader
	hash   hash.Hash
	offset int64
	buffer []byte
}

func (w *matchingWriter) Write(p []byte) (int, error) {
	if cap(w.buffer) < len(p) {
		w.buffer = make([]byte, len(p))
	}
	original := w.buffer[:len(p)]
	n, err := w.source.ReadAt(original, w.offset)
	if n != len(p) || !bytes.Equal(original, p) {
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return 0, errMismatch
	}
	_, _ = w.hash.Write(p)
	w.offset += int64(n)
	return len(p), nil
}
