package reconcile

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type SourceSummary struct {
	Path                 string `json:"path"`
	Bytes                int64  `json:"bytes"`
	Records              int64  `json:"records"`
	SHA256               string `json:"sha256"`
	FirstTimestamp       string `json:"first_timestamp"`
	LastTimestamp        string `json:"last_timestamp"`
	TimestampRegressions int64  `json:"timestamp_regressions"`
}

type Result struct {
	Base              SourceSummary `json:"base"`
	Branch            SourceSummary `json:"branch"`
	SharedRecords     int64         `json:"shared_records"`
	BaseOnlyRecords   int64         `json:"base_only_records"`
	AddedFromBranch   int64         `json:"added_from_branch"`
	OutputRecords     int64         `json:"output_records"`
	OutputBytes       int64         `json:"output_bytes,omitempty"`
	OutputSHA256      string        `json:"output_sha256,omitempty"`
	OutputPath        string        `json:"output_path,omitempty"`
	OutputRegressions int64         `json:"output_timestamp_regressions,omitempty"`
}

type recordKey struct {
	digest [sha256.Size]byte
	length int64
}

type recordRef struct {
	source    int
	sequence  int64
	offset    int64
	length    int64
	timestamp time.Time
	key       recordKey
}

type scannedSource struct {
	summary SourceSummary
	records []recordRef
	counts  map[recordKey]int64
}

func Analyze(basePath, branchPath string) (Result, error) {
	base, err := scanPath(basePath, 0)
	if err != nil {
		return Result{}, fmt.Errorf("scan base: %w", err)
	}
	branch, err := scanPath(branchPath, 1)
	if err != nil {
		return Result{}, fmt.Errorf("scan branch: %w", err)
	}
	result, _ := reconcileRecords(base, branch)
	return result, nil
}

func Merge(basePath, branchPath, outputPath string) (Result, error) {
	if outputPath == "" {
		return Result{}, errors.New("output path is required")
	}
	baseAbs, err := filepath.Abs(basePath)
	if err != nil {
		return Result{}, err
	}
	branchAbs, err := filepath.Abs(branchPath)
	if err != nil {
		return Result{}, err
	}
	outputAbs, err := filepath.Abs(outputPath)
	if err != nil {
		return Result{}, err
	}
	if outputAbs == baseAbs || outputAbs == branchAbs {
		return Result{}, errors.New("output must not replace either source")
	}
	if _, err := os.Lstat(outputAbs); err == nil {
		return Result{}, fmt.Errorf("output already exists: %s", outputAbs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}

	base, err := scanPath(baseAbs, 0)
	if err != nil {
		return Result{}, fmt.Errorf("scan base: %w", err)
	}
	branch, err := scanPath(branchAbs, 1)
	if err != nil {
		return Result{}, fmt.Errorf("scan branch: %w", err)
	}
	result, records := reconcileRecords(base, branch)
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].timestamp.Equal(records[j].timestamp) {
			if records[i].source == records[j].source {
				return records[i].sequence < records[j].sequence
			}
			return records[i].source < records[j].source
		}
		return records[i].timestamp.Before(records[j].timestamp)
	})

	baseFile, err := os.Open(baseAbs)
	if err != nil {
		return Result{}, err
	}
	defer baseFile.Close()
	branchFile, err := os.Open(branchAbs)
	if err != nil {
		return Result{}, err
	}
	defer branchFile.Close()

	if err := os.MkdirAll(filepath.Dir(outputAbs), 0o700); err != nil {
		return Result{}, err
	}
	temp, err := os.CreateTemp(filepath.Dir(outputAbs), ".codexfold-reconcile-*.tmp")
	if err != nil {
		return Result{}, err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = temp.Close()
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return Result{}, err
	}

	outputHasher := sha256.New()
	writer := io.MultiWriter(temp, outputHasher)
	for _, record := range records {
		source := baseFile
		if record.source == 1 {
			source = branchFile
		}
		if _, err := io.Copy(writer, io.NewSectionReader(source, record.offset, record.length)); err != nil {
			return Result{}, fmt.Errorf("write merged record: %w", err)
		}
	}
	if err := temp.Sync(); err != nil {
		return Result{}, err
	}
	if err := temp.Close(); err != nil {
		return Result{}, err
	}
	if err := verifyUnchanged(baseAbs, base.summary); err != nil {
		return Result{}, fmt.Errorf("base changed during merge: %w", err)
	}
	if err := verifyUnchanged(branchAbs, branch.summary); err != nil {
		return Result{}, fmt.Errorf("branch changed during merge: %w", err)
	}
	if err := os.Rename(tempPath, outputAbs); err != nil {
		return Result{}, err
	}
	committed = true
	if err := syncDir(filepath.Dir(outputAbs)); err != nil {
		return Result{}, err
	}

	output, err := scanPath(outputAbs, 2)
	if err != nil {
		return Result{}, fmt.Errorf("verify output: %w", err)
	}
	result.OutputPath = outputAbs
	result.OutputBytes = output.summary.Bytes
	result.OutputSHA256 = hex.EncodeToString(outputHasher.Sum(nil))
	result.OutputRegressions = output.summary.TimestampRegressions
	if output.summary.Records != result.OutputRecords || output.summary.SHA256 != result.OutputSHA256 || output.summary.TimestampRegressions != 0 {
		return Result{}, errors.New("merged output verification failed")
	}
	return result, nil
}

func reconcileRecords(base, branch scannedSource) (Result, []recordRef) {
	remaining := make(map[recordKey]int64, len(base.counts))
	for key, count := range base.counts {
		remaining[key] = count
	}
	records := make([]recordRef, 0, len(base.records)+len(branch.records))
	records = append(records, base.records...)
	var shared int64
	var added int64
	for _, record := range branch.records {
		if remaining[record.key] > 0 {
			remaining[record.key]--
			shared++
			continue
		}
		records = append(records, record)
		added++
	}
	return Result{
		Base:            base.summary,
		Branch:          branch.summary,
		SharedRecords:   shared,
		BaseOnlyRecords: base.summary.Records - shared,
		AddedFromBranch: added,
		OutputRecords:   base.summary.Records + added,
	}, records
}

func scanPath(path string, source int) (scannedSource, error) {
	file, err := os.Open(path)
	if err != nil {
		return scannedSource{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return scannedSource{}, err
	}

	result := scannedSource{
		summary: SourceSummary{Path: path},
		counts:  make(map[recordKey]int64),
	}
	reader := bufio.NewReaderSize(file, 1024*1024)
	fileHasher := sha256.New()
	var offset int64
	var previous time.Time
	for sequence := int64(0); ; sequence++ {
		start := offset
		recordHasher := sha256.New()
		timestampExtractor := newTimestampExtractor()
		hasData := false
		reachedEOF := false
		for {
			fragment, readErr := reader.ReadSlice('\n')
			if len(fragment) > 0 {
				hasData = true
				offset += int64(len(fragment))
				_, _ = fileHasher.Write(fragment)
				_, _ = recordHasher.Write(fragment)
				if err := timestampExtractor.Write(fragment); err != nil {
					return scannedSource{}, err
				}
			}
			switch {
			case readErr == nil:
				goto recordComplete
			case errors.Is(readErr, bufio.ErrBufferFull):
				continue
			case errors.Is(readErr, io.EOF):
				reachedEOF = true
				goto recordComplete
			default:
				return scannedSource{}, readErr
			}
		}

	recordComplete:
		if !hasData {
			break
		}
		timestamp, err := timestampExtractor.Timestamp()
		if err != nil {
			return scannedSource{}, fmt.Errorf("record %d at byte %d: %w", sequence+1, start, err)
		}
		if !previous.IsZero() && timestamp.Before(previous) {
			result.summary.TimestampRegressions++
		}
		if result.summary.Records == 0 {
			result.summary.FirstTimestamp = timestamp.Format(time.RFC3339Nano)
		}
		previous = timestamp
		result.summary.LastTimestamp = timestamp.Format(time.RFC3339Nano)
		var digest [sha256.Size]byte
		copy(digest[:], recordHasher.Sum(nil))
		key := recordKey{digest: digest, length: offset - start}
		result.records = append(result.records, recordRef{
			source:    source,
			sequence:  sequence,
			offset:    start,
			length:    offset - start,
			timestamp: timestamp,
			key:       key,
		})
		result.counts[key]++
		result.summary.Records++
		if reachedEOF {
			break
		}
	}
	after, err := file.Stat()
	if err != nil {
		return scannedSource{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return scannedSource{}, errors.New("source changed while scanning")
	}
	result.summary.Bytes = offset
	result.summary.SHA256 = hex.EncodeToString(fileHasher.Sum(nil))
	return result, nil
}

type timestampExtractor struct {
	depth       int
	inString    bool
	escaped     bool
	capture     bool
	token       []byte
	topKey      string
	expectation extractorExpectation
	timestamp   string
}

type extractorExpectation uint8

const (
	expectKey extractorExpectation = iota
	expectColon
	expectValue
)

func newTimestampExtractor() *timestampExtractor {
	return &timestampExtractor{expectation: expectKey}
}

func (e *timestampExtractor) Write(data []byte) error {
	for _, char := range data {
		if e.inString {
			if e.escaped {
				e.escaped = false
				if e.capture {
					e.token = append(e.token, char)
				}
				continue
			}
			if char == '\\' {
				e.escaped = true
				continue
			}
			if char == '"' {
				e.inString = false
				if e.capture {
					switch e.expectation {
					case expectKey:
						e.topKey = string(e.token)
						e.expectation = expectColon
					case expectValue:
						if e.topKey == "timestamp" {
							e.timestamp = string(e.token)
						}
					}
				}
				e.token = e.token[:0]
				e.capture = false
				continue
			}
			if e.capture {
				e.token = append(e.token, char)
			}
			continue
		}

		switch char {
		case '"':
			e.inString = true
			if e.depth == 1 && (e.expectation == expectKey || (e.expectation == expectValue && e.topKey == "timestamp")) {
				e.capture = true
				e.token = e.token[:0]
			}
		case '{', '[':
			e.depth++
		case '}', ']':
			if e.depth > 0 {
				e.depth--
			}
		case ':':
			if e.depth == 1 && e.expectation == expectColon {
				e.expectation = expectValue
			}
		case ',':
			if e.depth == 1 {
				e.expectation = expectKey
				e.topKey = ""
			}
		}
	}
	return nil
}

func (e *timestampExtractor) Timestamp() (time.Time, error) {
	if e.timestamp == "" {
		return time.Time{}, errors.New("top-level timestamp not found in record")
	}
	timestamp, err := time.Parse(time.RFC3339Nano, e.timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp %q: %w", e.timestamp, err)
	}
	return timestamp, nil
}

func verifyUnchanged(path string, expected SourceSummary) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != expected.Bytes {
		return fmt.Errorf("size is %d, expected %d", info.Size(), expected.Bytes)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != expected.SHA256 {
		return fmt.Errorf("sha256 is %s, expected %s", actual, expected.SHA256)
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
