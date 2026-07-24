package fold

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	digestBytes       = 32
	digestChunkCount  = 1 << 20
	maxMergeRunInputs = 64
)

type diskDigestSet struct {
	directory string
	input     *os.File
	closed    bool
}

func newDiskDigestSet() (*diskDigestSet, error) {
	directory, err := os.MkdirTemp("", "codexfold-digests-")
	if err != nil {
		return nil, err
	}
	input, err := os.OpenFile(filepath.Join(directory, "input.bin"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	return &diskDigestSet{directory: directory, input: input}, nil
}

func (s *diskDigestSet) Add(value string) error {
	if s.closed {
		return errors.New("digest set is closed")
	}
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != digestBytes {
		return fmt.Errorf("invalid SHA-256 %q", value)
	}
	_, err = s.input.Write(digest)
	return err
}

func (s *diskDigestSet) Count(ctx context.Context) (int, error) {
	if s.closed {
		return 0, errors.New("digest set is closed")
	}
	if err := s.input.Sync(); err != nil {
		return 0, err
	}
	if _, err := s.input.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	runs, err := s.createRuns(ctx)
	if err != nil {
		return 0, err
	}
	for len(runs) > maxMergeRunInputs {
		next := make([]string, 0, (len(runs)+maxMergeRunInputs-1)/maxMergeRunInputs)
		for start := 0; start < len(runs); start += maxMergeRunInputs {
			end := min(start+maxMergeRunInputs, len(runs))
			path := filepath.Join(s.directory, fmt.Sprintf("merged-%06d.bin", len(next)))
			if _, err := mergeDigestRuns(ctx, runs[start:end], path); err != nil {
				return 0, err
			}
			next = append(next, path)
			for _, old := range runs[start:end] {
				_ = os.Remove(old)
			}
		}
		runs = next
	}
	if len(runs) == 0 {
		return 0, nil
	}
	return mergeDigestRuns(ctx, runs, "")
}

func (s *diskDigestSet) createRuns(ctx context.Context) ([]string, error) {
	reader := bufio.NewReaderSize(s.input, 1<<20)
	runs := make([]string, 0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values := make([][digestBytes]byte, 0, digestChunkCount)
		for len(values) < digestChunkCount {
			var digest [digestBytes]byte
			_, err := io.ReadFull(reader, digest[:])
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			values = append(values, digest)
		}
		if len(values) == 0 {
			break
		}
		sort.Slice(values, func(left, right int) bool { return bytes.Compare(values[left][:], values[right][:]) < 0 })
		path := filepath.Join(s.directory, fmt.Sprintf("run-%06d.bin", len(runs)))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		writer := bufio.NewWriterSize(file, 1<<20)
		var previous [digestBytes]byte
		for index, digest := range values {
			if index > 0 && digest == previous {
				continue
			}
			if _, err := writer.Write(digest[:]); err != nil {
				_ = file.Close()
				return nil, err
			}
			previous = digest
		}
		if err := writer.Flush(); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		runs = append(runs, path)
	}
	return runs, nil
}

type digestRun struct {
	file    *os.File
	reader  *bufio.Reader
	digest  [digestBytes]byte
	ordinal int
}

type digestHeap []*digestRun

func (h digestHeap) Len() int { return len(h) }
func (h digestHeap) Less(left, right int) bool {
	comparison := bytes.Compare(h[left].digest[:], h[right].digest[:])
	return comparison < 0 || (comparison == 0 && h[left].ordinal < h[right].ordinal)
}
func (h digestHeap) Swap(left, right int) { h[left], h[right] = h[right], h[left] }
func (h *digestHeap) Push(value any)      { *h = append(*h, value.(*digestRun)) }
func (h *digestHeap) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

func mergeDigestRuns(ctx context.Context, paths []string, outputPath string) (int, error) {
	var output *os.File
	var writer *bufio.Writer
	if outputPath != "" {
		file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		output = file
		writer = bufio.NewWriterSize(file, 1<<20)
		defer output.Close()
	}
	runs := make([]*digestRun, 0, len(paths))
	defer func() {
		for _, run := range runs {
			_ = run.file.Close()
		}
	}()
	queue := digestHeap{}
	for ordinal, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		run := &digestRun{file: file, reader: bufio.NewReaderSize(file, 64<<10), ordinal: ordinal}
		runs = append(runs, run)
		if _, err := io.ReadFull(run.reader, run.digest[:]); err == nil {
			heap.Push(&queue, run)
		} else if !errors.Is(err, io.EOF) {
			return 0, err
		}
	}
	count := 0
	var previous [digestBytes]byte
	havePrevious := false
	for queue.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		run := heap.Pop(&queue).(*digestRun)
		if !havePrevious || run.digest != previous {
			count++
			previous = run.digest
			havePrevious = true
			if writer != nil {
				if _, err := writer.Write(run.digest[:]); err != nil {
					return 0, err
				}
			}
		}
		if _, err := io.ReadFull(run.reader, run.digest[:]); err == nil {
			heap.Push(&queue, run)
		} else if !errors.Is(err, io.EOF) {
			return 0, err
		}
	}
	if writer != nil {
		if err := writer.Flush(); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (s *diskDigestSet) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.input.Close()
	return errors.Join(err, os.RemoveAll(s.directory))
}
