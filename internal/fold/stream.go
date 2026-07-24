package fold

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

func (s *ObjectStore) OpenObject(_ context.Context, ref ObjectRef) (io.ReadCloser, error) {
	return s.OpenStream(ref)
}

func (s *ObjectStore) HasObject(ref ObjectRef) bool {
	info, err := os.Stat(s.ObjectPath(ref.SHA256))
	return err == nil && info.Mode().IsRegular()
}

type objectStream struct {
	ref     ObjectRef
	file    *os.File
	decoder *zstd.Decoder
	hash    hash.Hash
	read    int64
	done    bool
}

func (s *ObjectStore) OpenStream(ref ObjectRef) (io.ReadCloser, error) {
	file, err := os.Open(s.ObjectPath(ref.SHA256))
	if err != nil {
		return nil, fmt.Errorf("open object %s: %w", ref.SHA256, err)
	}
	decoder, err := zstd.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("create object stream %s: %w", ref.SHA256, err)
	}
	return &objectStream{ref: ref, file: file, decoder: decoder, hash: sha256.New()}, nil
}

func (s *objectStream) Read(destination []byte) (int, error) {
	n, err := s.decoder.Read(destination)
	if n > 0 {
		_, _ = s.hash.Write(destination[:n])
		s.read += int64(n)
	}
	if err == io.EOF && !s.done {
		s.done = true
		if s.read != s.ref.RawBytes {
			return n, fmt.Errorf("object %s raw size %d, want %d", s.ref.SHA256, s.read, s.ref.RawBytes)
		}
		if hex.EncodeToString(s.hash.Sum(nil)) != s.ref.SHA256 {
			return n, fmt.Errorf("object %s SHA-256 mismatch", s.ref.SHA256)
		}
	}
	return n, err
}

func (s *objectStream) Close() error {
	s.decoder.Close()
	return s.file.Close()
}
