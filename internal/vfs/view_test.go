package vfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"testing"

	"github.com/samekind/codexfold/internal/fold"
)

func TestViewReadsExactBytesAcrossPartBoundaries(t *testing.T) {
	view, source := fixtureView(t)
	buffer := make([]byte, 19)
	n, err := view.ReadAt(context.Background(), buffer, 4)
	if err != nil {
		t.Fatalf("ReadAt returned error: %v", err)
	}
	if !bytes.Equal(buffer[:n], source[4:23]) {
		t.Fatalf("cross-part bytes differ: got=%q want=%q", buffer[:n], source[4:23])
	}
	if view.Size() != int64(len(source)) {
		t.Fatalf("Size = %d, want %d", view.Size(), len(source))
	}
}

func TestViewMatchesNativeBytesForTenThousandRandomReads(t *testing.T) {
	view, source := fixtureView(t)
	random := rand.New(rand.NewSource(42))
	for iteration := 0; iteration < 10000; iteration++ {
		offset := random.Intn(len(source) + 5)
		length := random.Intn(80)
		buffer := make([]byte, length)
		n, err := view.ReadAt(context.Background(), buffer, int64(offset))
		if length == 0 {
			if n != 0 || err != nil {
				t.Fatalf("iteration %d zero read = (%d, %v)", iteration, n, err)
			}
			continue
		}
		if offset >= len(source) {
			if n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("iteration %d past EOF = (%d, %v)", iteration, n, err)
			}
			continue
		}
		end := offset + length
		if end > len(source) {
			end = len(source)
		}
		if !bytes.Equal(buffer[:n], source[offset:end]) {
			t.Fatalf("iteration %d bytes differ offset=%d length=%d", iteration, offset, length)
		}
		if end < offset+length {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("iteration %d error = %v, want EOF", iteration, err)
			}
		} else if err != nil {
			t.Fatalf("iteration %d unexpected error: %v", iteration, err)
		}
	}
}

func TestViewRejectsInconsistentManifestLength(t *testing.T) {
	manifest := fold.Manifest{
		Version: fold.ManifestVersion,
		Kind:    fold.ManifestKind,
		Source:  fold.ManifestSource{Bytes: 5, SHA256: string(make([]byte, 64))},
		Parts: []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{
			SHA256:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RawBytes: 4,
		}}},
	}
	if _, err := NewView(manifest, memoryReader{}); err == nil {
		t.Fatal("NewView should reject inconsistent manifest bytes")
	}
}

func TestViewPropagatesCancellation(t *testing.T) {
	view, _ := fixtureView(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := view.ReadAt(ctx, make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAt error = %v, want context.Canceled", err)
	}
}

func TestViewReadsAdjacentRepeatedObjectsOncePerRequest(t *testing.T) {
	object := bytes.Repeat([]byte("repeated-object-"), 4096)
	digest := sha256.Sum256(object)
	ref := fold.ObjectRef{
		SHA256:   hex.EncodeToString(digest[:]),
		RawBytes: int64(len(object)),
	}
	const repeats = 512
	manifest := fold.Manifest{
		Version: fold.ManifestVersion,
		Kind:    fold.ManifestKind,
		Source:  fold.ManifestSource{Bytes: int64(len(object) * repeats)},
		Parts:   make([]fold.Part, repeats),
	}
	for index := range manifest.Parts {
		manifest.Parts[index] = fold.Part{Kind: fold.PartResidual, Object: ref}
	}
	reader := &countingMemoryReader{objects: memoryReader{ref.SHA256: object}}
	view, err := NewView(manifest, reader)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(object)*repeats)
	if n, err := view.ReadAt(context.Background(), got, 0); err != nil || n != len(got) {
		t.Fatalf("ReadAt repeated objects = %d, %v", n, err)
	}
	if reader.calls != 1 {
		t.Fatalf("object reader calls = %d, want 1", reader.calls)
	}
	for index := 0; index < repeats; index++ {
		start := index * len(object)
		if !bytes.Equal(got[start:start+len(object)], object) {
			t.Fatalf("repeated object %d changed", index)
		}
	}
}

type memoryReader map[string][]byte

type countingMemoryReader struct {
	objects memoryReader
	calls   int
}

func (r *countingMemoryReader) ReadAt(ctx context.Context, ref fold.ObjectRef, destination []byte, offset int64) (int, error) {
	r.calls++
	return r.objects.ReadAt(ctx, ref, destination, offset)
}

func (r memoryReader) ReadAt(_ context.Context, ref fold.ObjectRef, destination []byte, offset int64) (int, error) {
	data, ok := r[ref.SHA256]
	if !ok {
		return 0, errors.New("missing object")
	}
	if offset >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(destination, data[offset:])
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}

func fixtureView(t *testing.T) (*View, []byte) {
	t.Helper()
	parts := [][]byte{
		[]byte("alpha-"),
		bytes.Repeat([]byte("B"), 33),
		[]byte("-gamma-"),
		bytes.Repeat([]byte("delta"), 17),
	}
	reader := memoryReader{}
	manifest := fold.Manifest{Version: fold.ManifestVersion, Kind: fold.ManifestKind}
	var source []byte
	for _, data := range parts {
		digest := sha256.Sum256(data)
		hexDigest := hex.EncodeToString(digest[:])
		reader[hexDigest] = data
		manifest.Parts = append(manifest.Parts, fold.Part{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: hexDigest, RawBytes: int64(len(data))}})
		source = append(source, data...)
	}
	sourceDigest := sha256.Sum256(source)
	manifest.Source = fold.ManifestSource{Bytes: int64(len(source)), SHA256: hex.EncodeToString(sourceDigest[:])}
	view, err := NewView(manifest, reader)
	if err != nil {
		t.Fatalf("NewView returned error: %v", err)
	}
	return view, source
}
