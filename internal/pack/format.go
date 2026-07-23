package pack

import (
	"fmt"
	"path/filepath"
)

const (
	IndexVersion       = 2
	IndexKind          = "pack-v2"
	legacyIndexVersion = 1
	legacyIndexKind    = "pack-v1"
	EncodingZstd       = "zstd"
	EncodingRaw        = "raw"
	defaultBlockBytes  = int64(256 << 10)
	defaultPackBytes   = int64(512 << 20)
	defaultCacheBytes  = int64(64 << 20)
)

type Index struct {
	Version    int      `json:"version"`
	Kind       string   `json:"kind"`
	Generation string   `json:"generation"`
	CreatedAt  string   `json:"created_at"`
	BlockBytes int64    `json:"block_bytes"`
	Objects    []Object `json:"objects"`
}

type Object struct {
	SHA256   string  `json:"sha256"`
	RawBytes int64   `json:"raw_bytes"`
	Blocks   []Block `json:"blocks"`
}

type Block struct {
	Pack        string `json:"pack"`
	PackOffset  int64  `json:"pack_offset"`
	StoredBytes int64  `json:"stored_bytes"`
	RawOffset   int64  `json:"raw_offset"`
	RawBytes    int64  `json:"raw_bytes"`
	SHA256      string `json:"sha256"`
	Encoding    string `json:"encoding,omitempty"`
}

func validateIndex(index Index) error {
	current := index.Version == IndexVersion && index.Kind == IndexKind
	legacy := index.Version == legacyIndexVersion && index.Kind == legacyIndexKind
	if !current && !legacy {
		return fmt.Errorf("unsupported pack index version=%d kind=%q", index.Version, index.Kind)
	}
	if !safeGeneration(index.Generation) || index.BlockBytes <= 0 {
		return fmt.Errorf("invalid pack index generation or block size")
	}
	seen := make(map[string]struct{}, len(index.Objects))
	for objectIndex, object := range index.Objects {
		if len(object.SHA256) != 64 || object.RawBytes < 0 {
			return fmt.Errorf("invalid pack object %d", objectIndex)
		}
		if _, ok := seen[object.SHA256]; ok {
			return fmt.Errorf("duplicate pack object %s", object.SHA256)
		}
		seen[object.SHA256] = struct{}{}
		var expectedOffset int64
		for blockIndex, block := range object.Blocks {
			if filepath.Base(block.Pack) != block.Pack || block.Pack == "." || block.Pack == ".." || block.PackOffset < 0 || block.StoredBytes <= 0 || block.StoredBytes > maxInt64() || block.RawBytes <= 0 || block.RawOffset != expectedOffset || len(block.SHA256) != 64 {
				return fmt.Errorf("invalid block %d for object %s", blockIndex, object.SHA256)
			}
			if block.Encoding != EncodingZstd && (!current || block.Encoding != EncodingRaw) {
				return fmt.Errorf("invalid block encoding %q for object %s", block.Encoding, object.SHA256)
			}
			expectedOffset += block.RawBytes
		}
		if expectedOffset != object.RawBytes {
			return fmt.Errorf("object %s block bytes %d, want %d", object.SHA256, expectedOffset, object.RawBytes)
		}
	}
	return nil
}

func normalizeLegacyIndex(index *Index) {
	if index.Version != legacyIndexVersion || index.Kind != legacyIndexKind {
		return
	}
	for objectIndex := range index.Objects {
		for blockIndex := range index.Objects[objectIndex].Blocks {
			block := &index.Objects[objectIndex].Blocks[blockIndex]
			if block.Encoding == "" {
				block.Encoding = EncodingZstd
			}
		}
	}
}

func maxInt64() int64 { return int64(^uint(0) >> 1) }

func safeGeneration(generation string) bool {
	return generation != "" && generation != "." && generation != ".." && filepath.Base(generation) == generation
}
