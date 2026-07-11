package contain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const firstRecordCaptureLimit = 8 * 1024 * 1024

type Input struct {
	ID   string
	Path string
}

type Options struct {
	IgnoreSessionMeta bool
}

type Result struct {
	ContainedID          string `json:"contained_id,omitempty"`
	ContainerID          string `json:"container_id,omitempty"`
	ContainedPath        string `json:"contained_path"`
	ContainerPath        string `json:"container_path"`
	IgnoredSessionMeta   bool   `json:"ignored_session_meta"`
	Contained            bool   `json:"contained"`
	VerifiedExact        bool   `json:"verified_exact"`
	ContainedRecords     int64  `json:"contained_records"`
	ContainedBytes       int64  `json:"contained_bytes"`
	ContainerStartRecord int64  `json:"container_start_record,omitempty"`
	ContainerEndRecord   int64  `json:"container_end_record,omitempty"`
	ContainerStartByte   int64  `json:"container_start_byte,omitempty"`
	ContainerEndByte     int64  `json:"container_end_byte,omitempty"`
}

type recordFingerprint struct {
	digest [sha256.Size]byte
	size   int64
}

var errContainmentFound = errors.New("containment found")

func Check(ctx context.Context, contained Input, container Input, options Options) (Result, error) {
	if contained.Path == "" || container.Path == "" {
		return Result{}, errors.New("both contained and container rollout paths are required")
	}
	result := Result{
		ContainedID: contained.ID, ContainerID: container.ID,
		ContainedPath: contained.Path, ContainerPath: container.Path,
		IgnoredSessionMeta: options.IgnoreSessionMeta,
	}
	needle := make([]recordFingerprint, 0, 1024)
	var needleStart int64 = -1
	var needleEnd int64
	err := scanRecords(ctx, contained.Path, options.IgnoreSessionMeta, func(record recordInfo) error {
		if needleStart < 0 {
			needleStart = record.start
		}
		needleEnd = record.end
		needle = append(needle, record.fingerprint)
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("scan contained rollout: %w", err)
	}
	if len(needle) == 0 {
		return Result{}, errors.New("contained rollout has no comparable JSONL records")
	}
	result.ContainedRecords = int64(len(needle))
	result.ContainedBytes = needleEnd - needleStart
	prefix := buildPrefixTable(needle)
	matched := 0
	err = scanRecords(ctx, container.Path, options.IgnoreSessionMeta, func(record recordInfo) error {
		for matched > 0 && !sameFingerprint(needle[matched], record.fingerprint) {
			matched = prefix[matched-1]
		}
		if sameFingerprint(needle[matched], record.fingerprint) {
			matched++
		}
		if matched != len(needle) {
			return nil
		}
		startByte := record.end - result.ContainedBytes
		exact, err := equalFileRanges(ctx, contained.Path, needleStart, container.Path, startByte, result.ContainedBytes)
		if err != nil {
			return err
		}
		if exact {
			result.Contained = true
			result.VerifiedExact = true
			result.ContainerStartRecord = record.index - int64(len(needle)) + 1
			result.ContainerEndRecord = record.index
			result.ContainerStartByte = startByte
			result.ContainerEndByte = record.end
			return errContainmentFound
		}
		matched = prefix[matched-1]
		return nil
	})
	if err != nil && !errors.Is(err, errContainmentFound) {
		return Result{}, fmt.Errorf("scan container rollout: %w", err)
	}
	return result, nil
}

type recordInfo struct {
	index       int64
	start       int64
	end         int64
	fingerprint recordFingerprint
}

func scanRecords(ctx context.Context, path string, ignoreSessionMeta bool, visit func(recordInfo) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReaderSize(file, 1024*1024)
	var offset int64
	var recordIndex int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hasher := sha256.New()
		start := offset
		var size int64
		var capture []byte
		captureComplete := true
		hasData := false
		reachedEOF := false
		for {
			fragment, readErr := reader.ReadSlice('\n')
			if len(fragment) > 0 {
				hasData = true
				_, _ = hasher.Write(fragment)
				size += int64(len(fragment))
				offset += int64(len(fragment))
				if recordIndex == 0 && captureComplete {
					if len(capture)+len(fragment) <= firstRecordCaptureLimit {
						capture = append(capture, fragment...)
					} else {
						capture = nil
						captureComplete = false
					}
				}
			}
			switch {
			case readErr == nil:
				goto recordComplete
			case errors.Is(readErr, bufio.ErrBufferFull):
				if err := ctx.Err(); err != nil {
					return err
				}
				continue
			case errors.Is(readErr, io.EOF):
				reachedEOF = true
				goto recordComplete
			default:
				return readErr
			}
		}

	recordComplete:
		if !hasData {
			return nil
		}
		var digest [sha256.Size]byte
		copy(digest[:], hasher.Sum(nil))
		skip := ignoreSessionMeta && recordIndex == 0 && captureComplete && isSessionMeta(capture)
		if !skip {
			if err := visit(recordInfo{
				index: recordIndex, start: start, end: offset,
				fingerprint: recordFingerprint{digest: digest, size: size},
			}); err != nil {
				return err
			}
		}
		recordIndex++
		if reachedEOF {
			return nil
		}
	}
}

func isSessionMeta(record []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(bytes.TrimSuffix(record, []byte{'\n'}), &envelope) == nil && envelope.Type == "session_meta"
}

func buildPrefixTable(records []recordFingerprint) []int {
	prefix := make([]int, len(records))
	for index, matched := 1, 0; index < len(records); index++ {
		for matched > 0 && !sameFingerprint(records[index], records[matched]) {
			matched = prefix[matched-1]
		}
		if sameFingerprint(records[index], records[matched]) {
			matched++
		}
		prefix[index] = matched
	}
	return prefix
}

func sameFingerprint(left recordFingerprint, right recordFingerprint) bool {
	return left.size == right.size && left.digest == right.digest
}

func equalFileRanges(ctx context.Context, leftPath string, leftOffset int64, rightPath string, rightOffset int64, size int64) (bool, error) {
	left, err := os.Open(leftPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = left.Close() }()
	right, err := os.Open(rightPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = right.Close() }()
	leftReader := io.NewSectionReader(left, leftOffset, size)
	rightReader := io.NewSectionReader(right, rightOffset, size)
	leftBuffer := make([]byte, 1024*1024)
	rightBuffer := make([]byte, len(leftBuffer))
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		chunk := int64(len(leftBuffer))
		if remaining < chunk {
			chunk = remaining
		}
		if _, err := io.ReadFull(leftReader, leftBuffer[:chunk]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(rightReader, rightBuffer[:chunk]); err != nil {
			return false, err
		}
		if !bytes.Equal(leftBuffer[:chunk], rightBuffer[:chunk]) {
			return false, nil
		}
		remaining -= chunk
	}
	return true, nil
}
