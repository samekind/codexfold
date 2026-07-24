package pack

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	indexV3Version      = 3
	indexV3Kind         = "pack-v3"
	indexV3MetaFilename = "index.meta.json"
	indexV3ObjectsFile  = "objects.idx"
	indexV3BlocksFile   = "blocks.idx"
	objectV3RecordBytes = 52
	blockV3RecordBytes  = 69
)

type indexV3Meta struct {
	Version     int      `json:"version"`
	Kind        string   `json:"kind"`
	Generation  string   `json:"generation"`
	CreatedAt   string   `json:"created_at"`
	BlockBytes  int64    `json:"block_bytes"`
	ObjectCount int64    `json:"object_count"`
	BlockCount  int64    `json:"block_count"`
	Packs       []string `json:"packs"`
}

type objectV3Record struct {
	Digest     [32]byte
	RawBytes   int64
	FirstBlock int64
	BlockCount uint32
}

type blockV3Record struct {
	PackIndex   uint32
	PackOffset  int64
	StoredBytes int64
	RawOffset   int64
	RawBytes    int64
	Digest      [32]byte
	Encoding    byte
}

type indexV3 struct {
	directory string
	meta      indexV3Meta
	objects   *os.File
	blocks    *os.File
}

func openIndexV3(directory string) (*indexV3, error) {
	data, err := os.ReadFile(filepath.Join(directory, indexV3MetaFilename))
	if err != nil {
		return nil, err
	}
	var meta indexV3Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decode pack v3 metadata: %w", err)
	}
	if err := validateIndexV3Meta(meta, filepath.Base(directory)); err != nil {
		return nil, err
	}
	objects, err := os.Open(filepath.Join(directory, indexV3ObjectsFile))
	if err != nil {
		return nil, fmt.Errorf("open pack v3 object index: %w", err)
	}
	blocks, err := os.Open(filepath.Join(directory, indexV3BlocksFile))
	if err != nil {
		_ = objects.Close()
		return nil, fmt.Errorf("open pack v3 block index: %w", err)
	}
	index := &indexV3{directory: directory, meta: meta, objects: objects, blocks: blocks}
	if err := index.validateSizes(); err != nil {
		_ = index.close()
		return nil, err
	}
	return index, nil
}

func validateIndexV3Meta(meta indexV3Meta, directoryName string) error {
	if meta.Version != indexV3Version || meta.Kind != indexV3Kind {
		return fmt.Errorf("unsupported pack v3 metadata version=%d kind=%q", meta.Version, meta.Kind)
	}
	if !safeGeneration(meta.Generation) || meta.BlockBytes <= 0 || meta.ObjectCount < 0 || meta.BlockCount < 0 {
		return errors.New("invalid pack v3 metadata")
	}
	if directoryName != meta.Generation && !strings.HasPrefix(directoryName, ".generation-") {
		return fmt.Errorf("pack index generation %q does not match directory %q", meta.Generation, directoryName)
	}
	seen := make(map[string]struct{}, len(meta.Packs))
	for _, name := range meta.Packs {
		if filepath.Base(name) != name || name == "." || name == ".." {
			return fmt.Errorf("unsafe pack filename %q", name)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate pack filename %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func (i *indexV3) validateSizes() error {
	objects, err := i.objects.Stat()
	if err != nil {
		return err
	}
	blocks, err := i.blocks.Stat()
	if err != nil {
		return err
	}
	if i.meta.ObjectCount > int64(^uint64(0)>>1)/objectV3RecordBytes || objects.Size() != i.meta.ObjectCount*objectV3RecordBytes {
		return errors.New("pack v3 object index size mismatch")
	}
	if i.meta.BlockCount > int64(^uint64(0)>>1)/blockV3RecordBytes || blocks.Size() != i.meta.BlockCount*blockV3RecordBytes {
		return errors.New("pack v3 block index size mismatch")
	}
	return nil
}

func (i *indexV3) lookup(digest string) (Object, bool, error) {
	want, err := decodeDigest(digest)
	if err != nil {
		return Object{}, false, err
	}
	left, right := int64(0), i.meta.ObjectCount
	for left < right {
		middle := left + (right-left)/2
		record, err := i.readObjectRecord(middle)
		if err != nil {
			return Object{}, false, err
		}
		comparison := bytes.Compare(record.Digest[:], want[:])
		if comparison < 0 {
			left = middle + 1
		} else {
			right = middle
		}
	}
	if left >= i.meta.ObjectCount {
		return Object{}, false, nil
	}
	record, err := i.readObjectRecord(left)
	if err != nil {
		return Object{}, false, err
	}
	if record.Digest != want {
		return Object{}, false, nil
	}
	object, err := i.expandObject(record)
	return object, err == nil, err
}

func (i *indexV3) objectAt(position int64) (Object, error) {
	record, err := i.readObjectRecord(position)
	if err != nil {
		return Object{}, err
	}
	return i.expandObject(record)
}

func (i *indexV3) readObjectRecord(position int64) (objectV3Record, error) {
	if position < 0 || position >= i.meta.ObjectCount {
		return objectV3Record{}, io.EOF
	}
	var data [objectV3RecordBytes]byte
	if _, err := i.objects.ReadAt(data[:], position*objectV3RecordBytes); err != nil {
		return objectV3Record{}, err
	}
	var record objectV3Record
	copy(record.Digest[:], data[:32])
	record.RawBytes = int64(binary.BigEndian.Uint64(data[32:40]))
	record.FirstBlock = int64(binary.BigEndian.Uint64(data[40:48]))
	record.BlockCount = binary.BigEndian.Uint32(data[48:52])
	return record, nil
}

func (i *indexV3) expandObject(record objectV3Record) (Object, error) {
	if record.RawBytes < 0 || record.FirstBlock < 0 || int64(record.BlockCount) > i.meta.BlockCount-record.FirstBlock {
		return Object{}, errors.New("invalid pack v3 object record")
	}
	object := Object{SHA256: hex.EncodeToString(record.Digest[:]), RawBytes: record.RawBytes, Blocks: make([]Block, 0, record.BlockCount)}
	var expectedOffset int64
	for offset := uint32(0); offset < record.BlockCount; offset++ {
		block, err := i.readBlockRecord(record.FirstBlock + int64(offset))
		if err != nil {
			return Object{}, err
		}
		if int(block.PackIndex) >= len(i.meta.Packs) || block.PackOffset < 0 || block.StoredBytes <= 0 || block.RawBytes <= 0 || block.RawOffset != expectedOffset {
			return Object{}, fmt.Errorf("invalid pack v3 block for object %s", object.SHA256)
		}
		encoding := EncodingZstd
		if block.Encoding == 1 {
			encoding = EncodingRaw
		} else if block.Encoding != 0 {
			return Object{}, fmt.Errorf("invalid pack v3 block encoding %d", block.Encoding)
		}
		object.Blocks = append(object.Blocks, Block{Pack: i.meta.Packs[block.PackIndex], PackOffset: block.PackOffset, StoredBytes: block.StoredBytes, RawOffset: block.RawOffset, RawBytes: block.RawBytes, SHA256: hex.EncodeToString(block.Digest[:]), Encoding: encoding})
		expectedOffset += block.RawBytes
	}
	if expectedOffset != record.RawBytes {
		return Object{}, fmt.Errorf("object %s block bytes %d, want %d", object.SHA256, expectedOffset, record.RawBytes)
	}
	return object, nil
}

func (i *indexV3) readBlockRecord(position int64) (blockV3Record, error) {
	if position < 0 || position >= i.meta.BlockCount {
		return blockV3Record{}, io.EOF
	}
	var data [blockV3RecordBytes]byte
	if _, err := i.blocks.ReadAt(data[:], position*blockV3RecordBytes); err != nil {
		return blockV3Record{}, err
	}
	var record blockV3Record
	record.PackIndex = binary.BigEndian.Uint32(data[0:4])
	record.PackOffset = int64(binary.BigEndian.Uint64(data[4:12]))
	record.StoredBytes = int64(binary.BigEndian.Uint64(data[12:20]))
	record.RawOffset = int64(binary.BigEndian.Uint64(data[20:28]))
	record.RawBytes = int64(binary.BigEndian.Uint64(data[28:36]))
	copy(record.Digest[:], data[36:68])
	record.Encoding = data[68]
	return record, nil
}

func encodeObjectV3(record objectV3Record) []byte {
	data := make([]byte, objectV3RecordBytes)
	copy(data[:32], record.Digest[:])
	binary.BigEndian.PutUint64(data[32:40], uint64(record.RawBytes))
	binary.BigEndian.PutUint64(data[40:48], uint64(record.FirstBlock))
	binary.BigEndian.PutUint32(data[48:52], record.BlockCount)
	return data
}

func encodeBlockV3(record blockV3Record) []byte {
	data := make([]byte, blockV3RecordBytes)
	binary.BigEndian.PutUint32(data[0:4], record.PackIndex)
	binary.BigEndian.PutUint64(data[4:12], uint64(record.PackOffset))
	binary.BigEndian.PutUint64(data[12:20], uint64(record.StoredBytes))
	binary.BigEndian.PutUint64(data[20:28], uint64(record.RawOffset))
	binary.BigEndian.PutUint64(data[28:36], uint64(record.RawBytes))
	copy(data[36:68], record.Digest[:])
	data[68] = record.Encoding
	return data
}

func decodeDigest(value string) ([32]byte, error) {
	var digest [32]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(digest) {
		return digest, fmt.Errorf("invalid SHA-256 %q", value)
	}
	copy(digest[:], decoded)
	return digest, nil
}

func (i *indexV3) close() error {
	return errors.Join(i.objects.Close(), i.blocks.Close())
}
