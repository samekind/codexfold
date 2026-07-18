package mountfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	nativeAppendJournalVersion  = 1
	maxNativeAppendPendingBytes = 256 << 20
)

var errNativeAppendGap = errors.New("native append transaction contains an unfilled offset gap")

// nativeAppendJournalCheckpoint is nil in production. Integration tests use it
// in a subprocess to terminate exactly after the recovery journal is durable.
var nativeAppendJournalCheckpoint func(nativeAppendJournal, []byte)

type nativeAppendSegment struct {
	offset int64
	data   []byte
}

type nativeAppendState struct {
	mu          sync.Mutex
	path        string
	journalRoot string
	baseSize    int64
	visibleEnd  int64
	segments    []nativeAppendSegment
}

type nativeAppendJournal struct {
	Version    int    `json:"version"`
	TargetPath string `json:"target_path"`
	BaseSize   int64  `json:"base_size"`
	FinalSize  int64  `json:"final_size"`
	TailSHA256 string `json:"tail_sha256"`
}

func newNativeAppendState(path string, journalRoot string) (*nativeAppendState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("native append target is not a regular file")
	}
	return &nativeAppendState{
		path: filepath.Clean(path), journalRoot: filepath.Clean(journalRoot),
		baseSize: info.Size(), visibleEnd: info.Size(),
	}, nil
}

func (s *nativeAppendState) Stage(data []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative native append offset")
	}
	if len(data) == 0 {
		return 0, nil
	}
	originalBytes := len(data)
	s.mu.Lock()
	defer s.mu.Unlock()

	end, overflow := addInt64(offset, int64(len(data)))
	if overflow {
		return 0, errors.New("native append offset overflow")
	}
	if offset < s.baseSize {
		committedEnd := min(end, s.baseSize)
		committed := make([]byte, committedEnd-offset)
		file, err := os.Open(s.path)
		if err != nil {
			return 0, err
		}
		_, readErr := file.ReadAt(committed, offset)
		closeErr := file.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, readErr
		}
		if closeErr != nil {
			return 0, closeErr
		}
		overlap := int(committedEnd - offset)
		if !bytes.Equal(committed, data[:overlap]) {
			s.clearPendingLocked()
			return 0, errors.New("native append conflicts with committed bytes")
		}
		data = data[overlap:]
		offset = committedEnd
	}
	if len(data) == 0 {
		return originalBytes, nil
	}
	end, overflow = addInt64(offset, int64(len(data)))
	if overflow || end-s.baseSize > maxNativeAppendPendingBytes {
		s.clearPendingLocked()
		return 0, errors.New("native append transaction exceeds the pending byte limit")
	}
	if err := s.stageSegmentLocked(offset, data); err != nil {
		s.clearPendingLocked()
		return 0, err
	}
	if end > s.visibleEnd {
		s.visibleEnd = end
	}
	return originalBytes, nil
}

func (s *nativeAppendState) stageSegmentLocked(offset int64, data []byte) error {
	segments := append(append([]nativeAppendSegment(nil), s.segments...), nativeAppendSegment{
		offset: offset,
		data:   append([]byte(nil), data...),
	})
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].offset < segments[j].offset })
	normalized := make([]nativeAppendSegment, 0, len(segments))
	for _, segment := range segments {
		if len(normalized) == 0 {
			normalized = append(normalized, segment)
			continue
		}
		last := &normalized[len(normalized)-1]
		lastEnd := last.offset + int64(len(last.data))
		segmentEnd := segment.offset + int64(len(segment.data))
		if segment.offset > lastEnd {
			normalized = append(normalized, segment)
			continue
		}
		overlapEnd := min(lastEnd, segmentEnd)
		if overlapEnd > segment.offset {
			lastStart := segment.offset - last.offset
			overlap := overlapEnd - segment.offset
			if !bytes.Equal(last.data[int(lastStart):int(lastStart+overlap)], segment.data[:int(overlap)]) {
				return errors.New("native append segments contain conflicting overlap")
			}
		}
		if segmentEnd > lastEnd {
			last.data = append(last.data, segment.data[int(lastEnd-segment.offset):]...)
		}
	}
	s.segments = normalized
	return nil
}

func (s *nativeAppendState) ReadAt(file *os.File, destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative native read offset")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	visibleEnd := s.contiguousEndLocked()
	if offset >= visibleEnd {
		return 0, io.EOF
	}
	limit := len(destination)
	if remaining := visibleEnd - offset; int64(limit) > remaining {
		limit = int(remaining)
	}
	visible := destination[:limit]
	clear(visible)
	if offset < s.baseSize {
		committedBytes := limit
		if remaining := s.baseSize - offset; int64(committedBytes) > remaining {
			committedBytes = int(remaining)
		}
		n, err := file.ReadAt(visible[:committedBytes], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, err
		}
	}
	for _, segment := range s.segments {
		segmentEnd := segment.offset + int64(len(segment.data))
		readEnd := offset + int64(limit)
		start := max(offset, segment.offset)
		end := min(readEnd, segmentEnd)
		if start >= end {
			continue
		}
		copy(visible[start-offset:end-offset], segment.data[start-segment.offset:end-segment.offset])
	}
	if limit < len(destination) {
		return limit, io.EOF
	}
	return limit, nil
}

func (s *nativeAppendState) VisibleSize() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contiguousEndLocked()
}

func (s *nativeAppendState) HasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.segments) != 0
}

func (s *nativeAppendState) Commit() error {
	return s.commit(true)
}

func (s *nativeAppendState) CommitAvailable() error {
	return s.commit(false)
}

func (s *nativeAppendState) commit(strict bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.segments) == 0 {
		file, err := os.OpenFile(s.path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		return errors.Join(syncErr, closeErr)
	}

	tail, err := s.assembleLocked()
	if err != nil {
		if !strict && errors.Is(err, errNativeAppendGap) {
			return nil
		}
		s.clearPendingLocked()
		return err
	}
	if tail[len(tail)-1] != '\n' {
		if !strict {
			return nil
		}
		s.clearPendingLocked()
		return errors.New("native append transaction ends with an incomplete JSONL record")
	}
	if !utf8.Valid(tail) || !completeJSONL(tail) {
		s.clearPendingLocked()
		return errors.New("native append transaction is not complete valid JSONL")
	}
	if err := commitNativeAppend(s.path, s.journalRoot, s.baseSize, tail); err != nil {
		s.clearPendingLocked()
		return err
	}
	s.baseSize += int64(len(tail))
	s.clearPendingLocked()
	return nil
}

func (s *nativeAppendState) Truncate(size int64) error {
	if size < 0 {
		return errors.New("negative native truncate size")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.segments) != 0 {
		return errors.New("cannot truncate a native append transaction with pending writes")
	}
	if err := os.Truncate(s.path, size); err != nil {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	s.baseSize = size
	s.visibleEnd = size
	return nil
}

func (s *nativeAppendState) Refresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.segments) != 0 {
		return errors.New("cannot refresh native append state with pending writes")
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	s.baseSize = info.Size()
	s.visibleEnd = info.Size()
	return nil
}

func (s *nativeAppendState) RefreshIfIdle() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.segments) != 0 {
		return nil
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	s.baseSize = info.Size()
	s.visibleEnd = info.Size()
	return nil
}

func (s *nativeAppendState) Relocate(newPath string) error {
	if !filepath.IsAbs(newPath) {
		return errors.New("absolute native append relocation path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	newPath = filepath.Clean(newPath)
	if err := os.Rename(s.path, newPath); err != nil {
		return err
	}
	s.path = newPath
	return nil
}

func (s *nativeAppendState) assembleLocked() ([]byte, error) {
	if s.visibleEnd < s.baseSize || s.visibleEnd-s.baseSize > maxNativeAppendPendingBytes {
		return nil, errors.New("native append transaction has an invalid visible range")
	}
	tail := make([]byte, s.visibleEnd-s.baseSize)
	covered := make([]byte, len(tail))
	segments := append([]nativeAppendSegment(nil), s.segments...)
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].offset < segments[j].offset })
	for _, segment := range segments {
		start := segment.offset - s.baseSize
		if start < 0 || start+int64(len(segment.data)) > int64(len(tail)) {
			return nil, errors.New("native append segment lies outside the transaction range")
		}
		for index, value := range segment.data {
			position := int(start) + index
			if covered[position] != 0 && tail[position] != value {
				return nil, errors.New("native append segments contain conflicting overlap")
			}
			tail[position] = value
			covered[position] = 1
		}
	}
	if bytes.IndexByte(covered, 0) >= 0 {
		return nil, errNativeAppendGap
	}
	return tail, nil
}

func (s *nativeAppendState) contiguousEndLocked() int64 {
	end := s.baseSize
	for _, segment := range s.segments {
		if segment.offset > end {
			break
		}
		segmentEnd := segment.offset + int64(len(segment.data))
		if segmentEnd > end {
			end = segmentEnd
		}
	}
	return end
}

func (s *nativeAppendState) clearPendingLocked() {
	s.segments = nil
	s.visibleEnd = s.baseSize
}

func commitNativeAppend(path string, journalRoot string, baseSize int64, tail []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() != baseSize {
		return fmt.Errorf("native append backing changed: size=%d expected=%d", info.Size(), baseSize)
	}
	digest := sha256.Sum256(tail)
	record := nativeAppendJournal{
		Version: nativeAppendJournalVersion, TargetPath: filepath.Clean(path), BaseSize: baseSize,
		FinalSize: baseSize + int64(len(tail)), TailSHA256: hex.EncodeToString(digest[:]),
	}
	journalPath, err := writeNativeAppendJournal(journalRoot, record)
	if err != nil {
		return err
	}
	if nativeAppendJournalCheckpoint != nil {
		nativeAppendJournalCheckpoint(record, tail)
	}
	rollback := func(commitErr error) error {
		truncateErr := os.Truncate(path, baseSize)
		syncErr := syncNativePath(path)
		if truncateErr != nil || syncErr != nil {
			// Leave the journal in place so startup recovery can finish rollback.
			return errors.Join(commitErr, truncateErr, syncErr)
		}
		return errors.Join(commitErr, removeNativeAppendJournal(journalPath))
	}

	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return rollback(err)
	}
	n, writeErr := file.WriteAt(tail, baseSize)
	if writeErr == nil && n != len(tail) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return rollback(errors.Join(writeErr, syncErr, closeErr))
	}
	verified, err := hashNativeAppendTail(path, baseSize, int64(len(tail)))
	if err != nil || verified != record.TailSHA256 {
		return rollback(fmt.Errorf("verify native append transaction: digest=%s expected=%s err=%w", verified, record.TailSHA256, err))
	}
	return removeNativeAppendJournal(journalPath)
}

func writeNativeAppendJournal(root string, record nativeAppendJournal) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("absolute native append journal root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(record.TargetPath))
	finalPath := filepath.Join(root, hex.EncodeToString(digest[:])+".json")
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(root, ".native-append-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return "", err
	}
	if err := syncDirectory(root); err != nil {
		return "", err
	}
	return finalPath, nil
}

func removeNativeAppendJournal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func recoverNativeAppendTransactions(nativeRoot string, journalRoot string) error {
	entries, err := os.ReadDir(journalRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("native append journal contains an unexpected directory %q", entry.Name())
		}
		path := filepath.Join(journalRoot, entry.Name())
		if strings.HasPrefix(entry.Name(), ".native-append-") && strings.HasSuffix(entry.Name(), ".tmp") {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("native append journal contains an unexpected file %q", entry.Name())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record nativeAppendJournal
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("decode native append journal %s: %w", entry.Name(), err)
		}
		if err := validateNativeAppendJournal(nativeRoot, record); err != nil {
			return fmt.Errorf("validate native append journal %s: %w", entry.Name(), err)
		}
		info, err := os.Stat(record.TargetPath)
		if err != nil {
			return err
		}
		committed := false
		if info.Size() == record.FinalSize {
			digest, hashErr := hashNativeAppendTail(record.TargetPath, record.BaseSize, record.FinalSize-record.BaseSize)
			committed = hashErr == nil && digest == record.TailSHA256
		}
		if !committed {
			if info.Size() < record.BaseSize {
				return fmt.Errorf("native append target is shorter than its rollback size: %s", record.TargetPath)
			}
			if err := os.Truncate(record.TargetPath, record.BaseSize); err != nil {
				return err
			}
			if err := syncNativePath(record.TargetPath); err != nil {
				return err
			}
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return syncDirectory(journalRoot)
}

func validateNativeAppendJournal(nativeRoot string, record nativeAppendJournal) error {
	if record.Version != nativeAppendJournalVersion || record.BaseSize < 0 || record.FinalSize < record.BaseSize ||
		record.FinalSize-record.BaseSize > maxNativeAppendPendingBytes {
		return errors.New("native append journal metadata is invalid")
	}
	target := filepath.Clean(record.TargetPath)
	relative, err := filepath.Rel(filepath.Clean(nativeRoot), target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("native append journal target is outside the native root")
	}
	if !nativeTransactionPath("/" + filepath.ToSlash(relative)) {
		return errors.New("native append journal target is not a canonical session path")
	}
	digest, err := hex.DecodeString(record.TailSHA256)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("native append journal digest is invalid")
	}
	return nil
}

func hashNativeAppendTail(path string, offset int64, size int64) (string, error) {
	if offset < 0 || size < 0 {
		return "", errors.New("invalid native append hash range")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.CopyN(hasher, io.NewSectionReader(file, offset, size), size); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func syncNativePath(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(syncErr, closeErr)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func addInt64(left int64, right int64) (int64, bool) {
	if right > 0 && left > int64(^uint64(0)>>1)-right {
		return 0, true
	}
	return left + right, false
}
