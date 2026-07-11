package fsctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
)

type Readable interface {
	Size() int64
	ReadAt(context.Context, []byte, int64) (int, error)
}

type ShadowOptions struct {
	BlockBytes  int
	RandomReads int
	Seed        int64
}

type ShadowResult struct {
	NativePath    string `json:"native_path"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
	ComparedBytes int64  `json:"compared_bytes"`
	RandomReads   int    `json:"random_reads"`
	Verified      bool   `json:"verified"`
}

func Shadow(ctx context.Context, nativePath string, virtual Readable, options ShadowOptions) (ShadowResult, error) {
	if options.BlockBytes <= 0 {
		options.BlockBytes = 1 << 20
	}
	if options.RandomReads < 0 {
		return ShadowResult{}, errors.New("shadow random read count cannot be negative")
	}
	native, err := os.Open(nativePath)
	if err != nil {
		return ShadowResult{}, err
	}
	defer native.Close()
	info, err := native.Stat()
	if err != nil {
		return ShadowResult{}, err
	}
	if info.Size() != virtual.Size() {
		return ShadowResult{}, fmt.Errorf("shadow size mismatch native=%d virtual=%d", info.Size(), virtual.Size())
	}
	nativeHash := sha256.New()
	virtualHash := sha256.New()
	nativeBuffer := make([]byte, options.BlockBytes)
	virtualBuffer := make([]byte, options.BlockBytes)
	var offset int64
	for offset < info.Size() {
		if err := ctx.Err(); err != nil {
			return ShadowResult{}, err
		}
		need := options.BlockBytes
		if remaining := info.Size() - offset; int64(need) > remaining {
			need = int(remaining)
		}
		nativeN, nativeErr := native.ReadAt(nativeBuffer[:need], offset)
		virtualN, virtualErr := virtual.ReadAt(ctx, virtualBuffer[:need], offset)
		if nativeN != need || virtualN != need || (nativeErr != nil && !errors.Is(nativeErr, io.EOF)) || (virtualErr != nil && !errors.Is(virtualErr, io.EOF)) {
			return ShadowResult{}, fmt.Errorf("shadow read failed at offset %d: native=(%d,%v) virtual=(%d,%v)", offset, nativeN, nativeErr, virtualN, virtualErr)
		}
		if !bytes.Equal(nativeBuffer[:need], virtualBuffer[:need]) {
			return ShadowResult{}, fmt.Errorf("shadow byte mismatch at offset %d", offset)
		}
		_, _ = nativeHash.Write(nativeBuffer[:need])
		_, _ = virtualHash.Write(virtualBuffer[:need])
		offset += int64(need)
	}
	nativeDigest := hex.EncodeToString(nativeHash.Sum(nil))
	if nativeDigest != hex.EncodeToString(virtualHash.Sum(nil)) {
		return ShadowResult{}, errors.New("shadow complete SHA-256 mismatch")
	}
	random := rand.New(rand.NewSource(options.Seed))
	for index := 0; index < options.RandomReads && info.Size() > 0; index++ {
		readOffset := random.Int63n(info.Size())
		length := 1 + random.Intn(options.BlockBytes)
		if remaining := info.Size() - readOffset; int64(length) > remaining {
			length = int(remaining)
		}
		nativeN, nativeErr := native.ReadAt(nativeBuffer[:length], readOffset)
		virtualN, virtualErr := virtual.ReadAt(ctx, virtualBuffer[:length], readOffset)
		if nativeN != length || virtualN != length || !bytes.Equal(nativeBuffer[:length], virtualBuffer[:length]) || (nativeErr != nil && !errors.Is(nativeErr, io.EOF)) || (virtualErr != nil && !errors.Is(virtualErr, io.EOF)) {
			return ShadowResult{}, fmt.Errorf("shadow random read %d differs at offset %d", index, readOffset)
		}
	}
	after, err := hashFile(nativePath)
	if err != nil {
		return ShadowResult{}, err
	}
	if after.Bytes != info.Size() || after.SHA256 != nativeDigest {
		return ShadowResult{}, errors.New("native source changed during shadow verification")
	}
	return ShadowResult{NativePath: nativePath, Bytes: info.Size(), SHA256: nativeDigest, ComparedBytes: offset, RandomReads: options.RandomReads, Verified: true}, nil
}

type fileDigest struct {
	Bytes  int64
	SHA256 string
}

func hashFile(path string) (fileDigest, error) {
	file, err := os.Open(path)
	if err != nil {
		return fileDigest{}, err
	}
	defer file.Close()
	hasher := sha256.New()
	bytesRead, err := io.Copy(hasher, file)
	if err != nil {
		return fileDigest{}, err
	}
	return fileDigest{Bytes: bytesRead, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}
