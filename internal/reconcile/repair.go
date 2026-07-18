package reconcile

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/jstar0/codexfold/internal/storage"
)

const maxRepairBufferedBytes = int64(64 * 1024 * 1024)

var recordStartPattern = regexp.MustCompile(`\{"timestamp"\s*:`)

type RepairResult struct {
	SourcePath                       string                      `json:"source_path"`
	SourceBytes                      int64                       `json:"source_bytes"`
	SourceSHA256                     string                      `json:"source_sha256"`
	PhysicalLines                    int64                       `json:"physical_lines"`
	InvalidPhysicalLines             int64                       `json:"invalid_physical_lines"`
	ReconstructedRecords             int64                       `json:"reconstructed_records"`
	SourceConversationRecords        int64                       `json:"source_conversation_records"`
	PreservedConversationRecords     int64                       `json:"preserved_conversation_records"`
	ReconstructedConversationRecords int64                       `json:"reconstructed_conversation_records"`
	ConversationIntegrityVerified    bool                        `json:"conversation_integrity_verified"`
	OutputPath                       string                      `json:"output_path"`
	OutputBytes                      int64                       `json:"output_bytes"`
	OutputRecords                    int64                       `json:"output_records"`
	OutputSHA256                     string                      `json:"output_sha256"`
	TimestampRegressions             int64                       `json:"timestamp_regressions"`
	MaximumBufferedBytes             int64                       `json:"maximum_buffered_bytes"`
	OrphanBytes                      int64                       `json:"orphan_bytes,omitempty"`
	OrphanLines                      int64                       `json:"orphan_lines,omitempty"`
	Storage                          *storage.MutationAccounting `json:"storage,omitempty"`
}

type RepairOptions struct {
	AllowOrphans bool
	OrphanPath   string
	Context      context.Context
	Budget       storage.Checker
}

type repairFrame struct {
	partial     []byte
	pending     []repairRecord
	startedLine int64
}

type repairRecord struct {
	data        []byte
	sourceValid bool
}

type repairWriter struct {
	writer                           io.Writer
	stack                            []repairFrame
	bufferedBytes                    int64
	maximumBuffered                  int64
	outputRecords                    int64
	reconstructed                    int64
	previousTimestamp                time.Time
	timestampRegressions             int64
	orphanWriter                     *bufio.Writer
	allowOrphans                     bool
	orphanBytes                      int64
	orphanLines                      int64
	sourceConversationChain          conversationChain
	preservedConversationChain       conversationChain
	sourceConversationRecords        int64
	preservedConversationRecords     int64
	reconstructedConversationRecords int64
}

func Repair(sourcePath, outputPath string) (RepairResult, error) {
	return RepairWithOptions(sourcePath, outputPath, RepairOptions{})
}

func RepairWithOptions(sourcePath, outputPath string, options RepairOptions) (RepairResult, error) {
	if outputPath == "" {
		return RepairResult{}, errors.New("output path is required")
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return RepairResult{}, err
	}
	outputAbs, err := filepath.Abs(outputPath)
	if err != nil {
		return RepairResult{}, err
	}
	if sourceAbs == outputAbs {
		return RepairResult{}, errors.New("output must not replace the source")
	}
	if _, err := os.Lstat(outputAbs); err == nil {
		return RepairResult{}, fmt.Errorf("output already exists: %s", outputAbs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return RepairResult{}, err
	}
	if options.AllowOrphans {
		if options.OrphanPath == "" {
			return RepairResult{}, errors.New("orphan path is required when allow orphans is enabled")
		}
		orphanAbs, err := filepath.Abs(options.OrphanPath)
		if err != nil {
			return RepairResult{}, err
		}
		if orphanAbs == sourceAbs || orphanAbs == outputAbs {
			return RepairResult{}, errors.New("orphan output must be separate from source and repaired output")
		}
		if _, err := os.Lstat(orphanAbs); err == nil {
			return RepairResult{}, fmt.Errorf("orphan output already exists: %s", orphanAbs)
		} else if !errors.Is(err, os.ErrNotExist) {
			return RepairResult{}, err
		}
		options.OrphanPath = orphanAbs
	}

	source, err := os.Open(sourceAbs)
	if err != nil {
		return RepairResult{}, err
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil {
		return RepairResult{}, err
	}
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	estimatedBytes, err := estimatedOutputBytes(before.Size())
	if err != nil {
		return RepairResult{}, err
	}
	persistentBytes := estimatedBytes
	if options.AllowOrphans {
		if persistentBytes > int64(^uint64(0)>>1)-estimatedBytes {
			return RepairResult{}, errors.New("repair output byte estimate overflow")
		}
		persistentBytes += estimatedBytes
	}
	budget := options.Budget
	if budget == nil {
		budget = storage.VolumeGuard{Path: filepath.Dir(outputAbs)}
	}
	storageAssessment, err := budget.Check(ctx, storage.Projection{
		Operation: "repair-rollout", AdditionalPersistentBytes: persistentBytes,
		TemporaryBytes: estimatedBytes, TemporaryPersistentOverlapBytes: estimatedBytes,
	})
	if err != nil {
		return RepairResult{}, err
	}
	if options.AllowOrphans && options.Budget == nil {
		if _, err := (storage.VolumeGuard{Path: filepath.Dir(options.OrphanPath)}).Check(ctx, storage.Projection{Operation: "repair-orphans", AdditionalPersistentBytes: estimatedBytes}); err != nil {
			return RepairResult{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(outputAbs), 0o700); err != nil {
		return RepairResult{}, err
	}
	temp, err := os.CreateTemp(filepath.Dir(outputAbs), ".codexfold-repair-*.tmp")
	if err != nil {
		return RepairResult{}, err
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
		return RepairResult{}, err
	}
	var orphanFile *os.File
	var orphanWriter *bufio.Writer
	if options.AllowOrphans {
		if err := os.MkdirAll(filepath.Dir(options.OrphanPath), 0o700); err != nil {
			return RepairResult{}, err
		}
		orphanFile, err = os.OpenFile(options.OrphanPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return RepairResult{}, err
		}
		defer orphanFile.Close()
		defer func() {
			if !committed {
				_ = os.Remove(options.OrphanPath)
			}
		}()
		orphanWriter = bufio.NewWriterSize(orphanFile, 64*1024)
	}

	result := RepairResult{SourcePath: sourceAbs, OutputPath: outputAbs}
	sourceHasher := sha256.New()
	outputHasher := sha256.New()
	processor := repairWriter{writer: io.MultiWriter(temp, outputHasher), orphanWriter: orphanWriter, allowOrphans: options.AllowOrphans}
	reader := bufio.NewReaderSize(source, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return RepairResult{}, err
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			result.PhysicalLines++
			result.SourceBytes += int64(len(line))
			_, _ = sourceHasher.Write(line)
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if json.Valid(line) {
				if err := processor.acceptValid(line); err != nil {
					return RepairResult{}, fmt.Errorf("physical line %d: %w", result.PhysicalLines, err)
				}
			} else {
				result.InvalidPhysicalLines++
				if err := processor.acceptFragment(line, result.PhysicalLines); err != nil {
					return RepairResult{}, fmt.Errorf("physical line %d: %w", result.PhysicalLines, err)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return RepairResult{}, readErr
		}
	}
	if len(processor.stack) != 0 && !options.AllowOrphans {
		return RepairResult{}, fmt.Errorf("unresolved interrupted record started at physical line %d", processor.stack[0].startedLine)
	}
	if len(processor.stack) != 0 {
		if err := processor.salvageUnresolved(); err != nil {
			return RepairResult{}, err
		}
	}
	if err := processor.verifyConversationIntegrity(); err != nil {
		return RepairResult{}, err
	}
	if processor.timestampRegressions != 0 && !options.AllowOrphans {
		return RepairResult{}, fmt.Errorf("repaired record order still has %d timestamp regressions", processor.timestampRegressions)
	}
	after, err := source.Stat()
	if err != nil {
		return RepairResult{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return RepairResult{}, errors.New("source changed while repairing")
	}
	if err := temp.Sync(); err != nil {
		return RepairResult{}, err
	}
	if err := temp.Close(); err != nil {
		return RepairResult{}, err
	}
	if orphanWriter != nil {
		if err := orphanWriter.Flush(); err != nil {
			return RepairResult{}, err
		}
		if err := orphanFile.Sync(); err != nil {
			return RepairResult{}, err
		}
	}
	verified, err := scanPath(tempPath, 2)
	if err != nil {
		return RepairResult{}, fmt.Errorf("verify repaired output: %w", err)
	}
	if (!options.AllowOrphans && verified.summary.TimestampRegressions != 0) || verified.summary.Records != processor.outputRecords {
		return RepairResult{}, errors.New("repaired output verification failed")
	}
	outputDigest := hex.EncodeToString(outputHasher.Sum(nil))
	if verified.summary.SHA256 != outputDigest {
		return RepairResult{}, errors.New("repaired output digest verification failed")
	}
	if err := os.Rename(tempPath, outputAbs); err != nil {
		return RepairResult{}, err
	}
	committed = true
	if err := syncDir(filepath.Dir(outputAbs)); err != nil {
		return RepairResult{}, err
	}

	result.SourceSHA256 = hex.EncodeToString(sourceHasher.Sum(nil))
	result.ReconstructedRecords = processor.reconstructed
	result.SourceConversationRecords = processor.sourceConversationRecords
	result.PreservedConversationRecords = processor.preservedConversationRecords
	result.ReconstructedConversationRecords = processor.reconstructedConversationRecords
	result.ConversationIntegrityVerified = true
	result.OutputBytes = verified.summary.Bytes
	result.OutputRecords = verified.summary.Records
	result.OutputSHA256 = outputDigest
	result.TimestampRegressions = verified.summary.TimestampRegressions
	result.MaximumBufferedBytes = processor.maximumBuffered
	result.OrphanBytes = processor.orphanBytes
	result.OrphanLines = processor.orphanLines
	result.Storage = storage.CompleteAccounting(ctx, storageAssessment, "")
	return result, nil
}

func (w *repairWriter) acceptValid(record []byte) error {
	w.trackSourceConversation(record)
	if len(w.stack) == 0 {
		return w.writeRecord(repairRecord{data: record, sourceValid: true})
	}
	copyOfRecord := append([]byte(nil), record...)
	top := &w.stack[len(w.stack)-1]
	top.pending = append(top.pending, repairRecord{data: copyOfRecord, sourceValid: true})
	return w.addBuffered(int64(len(copyOfRecord)))
}

func (w *repairWriter) acceptFragment(line []byte, physicalLine int64) error {
	starts := recordStartPattern.FindAllIndex(line, -1)
	if len(starts) == 0 {
		if len(w.stack) == 0 {
			return w.handleOrphan(line)
		}
		return w.appendFragment(line)
	}

	cursor := 0
	for _, match := range starts {
		start := match[0]
		if start > cursor {
			if len(w.stack) == 0 {
				if len(bytes.TrimSpace(line[cursor:start])) != 0 {
					if err := w.handleOrphan(line[cursor:start]); err != nil {
						return err
					}
				}
			} else if err := w.appendFragment(line[cursor:start]); err != nil {
				return err
			}
		}
		w.stack = append(w.stack, repairFrame{startedLine: physicalLine})
		cursor = start
	}
	return w.appendFragment(line[cursor:])
}

func (w *repairWriter) appendFragment(fragment []byte) error {
	if len(w.stack) == 0 {
		return w.handleOrphan(fragment)
	}
	top := &w.stack[len(w.stack)-1]
	top.partial = append(top.partial, fragment...)
	if err := w.addBuffered(int64(len(fragment))); err != nil {
		return err
	}
	if json.Valid(top.partial) {
		return w.finishTop()
	}
	return nil
}

func (w *repairWriter) handleOrphan(fragment []byte) error {
	if len(bytes.TrimSpace(fragment)) == 0 {
		return nil
	}
	if !w.allowOrphans {
		return errors.New("orphan JSON fragment has no active interrupted record")
	}
	return w.writeOrphan(fragment)
}

func (w *repairWriter) writeOrphan(fragment []byte) error {
	if w.orphanWriter == nil {
		return errors.New("orphan writer is not configured")
	}
	if containsCompleteConversationRecord(fragment) {
		return errors.New("refusing to orphan a complete user or assistant conversation record")
	}
	if _, err := w.orphanWriter.Write(fragment); err != nil {
		return err
	}
	if err := w.orphanWriter.WriteByte('\n'); err != nil {
		return err
	}
	w.orphanBytes += int64(len(fragment))
	w.orphanLines++
	return nil
}

func (w *repairWriter) finishTop() error {
	index := len(w.stack) - 1
	frame := w.stack[index]
	w.stack = w.stack[:index]
	w.reconstructed++
	records := make([]repairRecord, 0, 1+len(frame.pending))
	records = append(records, repairRecord{data: frame.partial})
	records = append(records, frame.pending...)
	if len(w.stack) != 0 {
		parent := &w.stack[len(w.stack)-1]
		parent.pending = append(parent.pending, records...)
		return nil
	}
	for _, record := range records {
		if err := w.writeRecord(record); err != nil {
			return err
		}
		w.bufferedBytes -= int64(len(record.data))
	}
	return nil
}

func (w *repairWriter) salvageUnresolved() error {
	for _, frame := range w.stack {
		if err := w.writeOrphan(frame.partial); err != nil {
			return err
		}
		for _, pending := range frame.pending {
			if err := w.writeRecord(pending); err != nil {
				return err
			}
		}
	}
	w.stack = nil
	w.bufferedBytes = 0
	return nil
}

func (w *repairWriter) writeRecord(record repairRecord) error {
	if !json.Valid(record.data) {
		return errors.New("attempted to emit invalid JSON record")
	}
	extractor := newTimestampExtractor()
	if err := extractor.Write(record.data); err != nil {
		return err
	}
	timestamp, err := extractor.Timestamp()
	if err != nil {
		return err
	}
	if !w.previousTimestamp.IsZero() && timestamp.Before(w.previousTimestamp) {
		w.timestampRegressions++
	}
	w.previousTimestamp = timestamp
	if _, err := w.writer.Write(record.data); err != nil {
		return err
	}
	if _, err := w.writer.Write([]byte{'\n'}); err != nil {
		return err
	}
	w.outputRecords++
	w.trackOutputConversation(record)
	return nil
}

func (w *repairWriter) trackSourceConversation(record []byte) {
	if conversationRecordKind(record) == "" {
		return
	}
	w.sourceConversationChain = w.sourceConversationChain.append(record)
	w.sourceConversationRecords++
}

func (w *repairWriter) trackOutputConversation(record repairRecord) {
	if conversationRecordKind(record.data) == "" {
		return
	}
	if record.sourceValid {
		w.preservedConversationChain = w.preservedConversationChain.append(record.data)
		w.preservedConversationRecords++
		return
	}
	w.reconstructedConversationRecords++
}

func (w *repairWriter) verifyConversationIntegrity() error {
	if w.sourceConversationRecords != w.preservedConversationRecords || w.sourceConversationChain != w.preservedConversationChain {
		return fmt.Errorf("conversation integrity verification failed: source=%d preserved=%d", w.sourceConversationRecords, w.preservedConversationRecords)
	}
	return nil
}

func (w *repairWriter) addBuffered(bytes int64) error {
	w.bufferedBytes += bytes
	if w.bufferedBytes > w.maximumBuffered {
		w.maximumBuffered = w.bufferedBytes
	}
	if w.bufferedBytes > maxRepairBufferedBytes {
		return fmt.Errorf("interrupted record buffer exceeded %d bytes", maxRepairBufferedBytes)
	}
	return nil
}
