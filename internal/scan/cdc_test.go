package scan

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestContentDefinedChunksRealignAfterInsertion(t *testing.T) {
	original := deterministicBytes(1024 * 1024)
	inserted := make([]byte, 0, len(original)+137)
	inserted = append(inserted, original[:170000]...)
	inserted = append(inserted, bytes.Repeat([]byte{0x5a}, 137)...)
	inserted = append(inserted, original[170000:]...)

	options := DedupCDCOptions{MinBytes: 4 * 1024, AverageBytes: 16 * 1024, MaxBytes: 64 * 1024}
	originalChunks := collectCDCChunks(t, original, options)
	insertedChunks := collectCDCChunks(t, inserted, options)
	shared := 0
	originalSet := make(map[[sha256.Size]byte]struct{}, len(originalChunks))
	for _, chunk := range originalChunks {
		originalSet[chunk.Digest] = struct{}{}
	}
	for _, chunk := range insertedChunks {
		if _, ok := originalSet[chunk.Digest]; ok {
			shared++
		}
	}
	if shared < len(originalChunks)*3/4 {
		t.Fatalf("shared chunks = %d of %d original chunks; content-defined boundaries did not realign", shared, len(originalChunks))
	}
}

func TestContentDefinedChunkBounds(t *testing.T) {
	options := DedupCDCOptions{MinBytes: 64, AverageBytes: 128, MaxBytes: 256}
	chunks := collectCDCChunks(t, deterministicBytes(32*1024+17), options)
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want multiple chunks", len(chunks))
	}
	for index, chunk := range chunks {
		if chunk.Size > options.MaxBytes {
			t.Fatalf("chunk %d size = %d, exceeds max %d", index, chunk.Size, options.MaxBytes)
		}
		if index != len(chunks)-1 && chunk.Size < options.MinBytes {
			t.Fatalf("chunk %d size = %d, below min %d", index, chunk.Size, options.MinBytes)
		}
	}
}

type testCDCChunk struct {
	Digest [sha256.Size]byte
	Size   int64
}

func collectCDCChunks(t *testing.T, data []byte, options DedupCDCOptions) []testCDCChunk {
	t.Helper()
	chunks := make([]testCDCChunk, 0)
	chunker, err := newDedupCDCChunker(options, func(digest [sha256.Size]byte, size int64) error {
		chunks = append(chunks, testCDCChunk{Digest: digest, Size: size})
		return nil
	})
	if err != nil {
		t.Fatalf("create CDC chunker: %v", err)
	}
	if err := chunker.Write(data); err != nil {
		t.Fatalf("write CDC data: %v", err)
	}
	if err := chunker.Finish(); err != nil {
		t.Fatalf("finish CDC data: %v", err)
	}
	return chunks
}

func deterministicBytes(size int) []byte {
	data := make([]byte, size)
	state := uint64(0x123456789abcdef0)
	for index := range data {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		data[index] = byte(state)
	}
	return data
}
