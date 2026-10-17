package mountfs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	nativeQuarantineDirectory = ".codexfold-native-quarantine"
	nativeRepairTempPrefix    = ".codexfold-repair-"
	nativeRepairTempSuffix    = ".tmp"
	nativeRepairCopyChunk     = 4 << 20
)

// nativeByteRange is a half-open [Start, End) span of a rollout file.
type nativeByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// NativeRolloutRepair describes one automatic repair of a damaged active
// rollout. Every removed byte is kept in QuarantinePath, so the original file
// can be rebuilt by re-inserting each range at its recorded offset.
type NativeRolloutRepair struct {
	Path            string `json:"path"`
	QuarantinePath  string `json:"quarantine_path,omitempty"`
	RemovedRanges   int    `json:"removed_ranges"`
	RemovedBytes    int64  `json:"removed_bytes"`
	AddedFinalLF    bool   `json:"added_final_newline,omitempty"`
	OriginalSize    int64  `json:"original_size"`
	RepairedSize    int64  `json:"repaired_size"`
	FirstDamagedAt  int64  `json:"first_damaged_offset"`
	QuarantineError string `json:"quarantine_error,omitempty"`
}

type nativeQuarantineManifest struct {
	Version      int                      `json:"version"`
	RelativePath string                   `json:"relative_path"`
	CreatedAt    string                   `json:"created_at"`
	OriginalSize int64                    `json:"original_size"`
	Reason       string                   `json:"reason"`
	Ranges       []nativeQuarantinedRange `json:"ranges"`
}

type nativeQuarantinedRange struct {
	nativeByteRange
	SHA256 string `json:"sha256"`
	File   string `json:"file"`
}

type nativeDamage struct {
	ranges []nativeByteRange
	// finalNewline is set when the file ends with a complete JSON record that
	// only lacks its newline, which is repaired by appending one.
	finalNewline bool
	size         int64
}

func (d nativeDamage) any() bool { return len(d.ranges) != 0 || d.finalNewline }

// scanNativeDamage reports the lines at or after from that are not valid
// UTF-8 JSON records. from must be a record boundary. Adjacent damaged lines
// are merged, and a trailing partial line is damaged unless it is a complete
// JSON record without its newline.
func scanNativeDamage(ctx context.Context, filePath string, from int64) (nativeDamage, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nativeDamage{}, err
	}
	defer file.Close()
	if from > 0 {
		boundary := []byte{0}
		if _, err := file.ReadAt(boundary, from-1); err != nil || boundary[0] != '\n' {
			return nativeDamage{}, fmt.Errorf("native repair offset %d is not a JSONL boundary in %s", from, filePath)
		}
	}
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		return nativeDamage{}, err
	}
	reader := bufio.NewReaderSize(file, 1<<20)
	damage := nativeDamage{}
	offset := from
	add := func(start int64, end int64) {
		if last := len(damage.ranges) - 1; last >= 0 && damage.ranges[last].End == start {
			damage.ranges[last].End = end
			return
		}
		damage.ranges = append(damage.ranges, nativeByteRange{Start: start, End: end})
	}
	for {
		if err := ctx.Err(); err != nil {
			return nativeDamage{}, err
		}
		line, readErr := reader.ReadBytes('\n')
		start := offset
		offset += int64(len(line))
		if errors.Is(readErr, io.EOF) {
			if len(line) != 0 {
				if utf8.Valid(line) && json.Valid(line) {
					damage.finalNewline = true
				} else {
					add(start, offset)
				}
			}
			break
		}
		if readErr != nil {
			return nativeDamage{}, readErr
		}
		if !utf8.Valid(line) || !json.Valid(line) {
			add(start, offset)
		}
	}
	damage.size = offset
	return damage, nil
}

// repairNativeRollout removes damaged records from one active rollout. The
// removed bytes are written durably to the quarantine before the file is
// touched, and the repaired content replaces the rollout through a same-volume
// rename of a clone, so a crash or a full disk leaves the original in place.
// The caller must make sure nothing is writing the rollout.
func repairNativeRollout(ctx context.Context, nativeRoot string, filePath string, from int64) (NativeRolloutRepair, bool, error) {
	damage, err := scanNativeDamage(ctx, filePath, from)
	if err != nil {
		return NativeRolloutRepair{}, false, err
	}
	if !damage.any() {
		return NativeRolloutRepair{}, false, nil
	}
	before, err := os.Lstat(filePath)
	if err != nil {
		return NativeRolloutRepair{}, false, err
	}
	if !before.Mode().IsRegular() || before.Size() != damage.size {
		return NativeRolloutRepair{}, false, fmt.Errorf("native rollout changed before repair: %s", filePath)
	}
	relative, err := filepath.Rel(filepath.Clean(nativeRoot), filePath)
	if err != nil || strings.HasPrefix(relative, "..") {
		return NativeRolloutRepair{}, false, fmt.Errorf("native repair path escaped native root: %s", filePath)
	}
	repair := NativeRolloutRepair{
		Path: filePath, RemovedRanges: len(damage.ranges), AddedFinalLF: damage.finalNewline,
		OriginalSize: damage.size, FirstDamagedAt: damage.size,
	}
	for _, span := range damage.ranges {
		repair.RemovedBytes += span.End - span.Start
		repair.FirstDamagedAt = min(repair.FirstDamagedAt, span.Start)
	}
	repair.RepairedSize = damage.size - repair.RemovedBytes
	if damage.finalNewline {
		repair.RepairedSize++
	}

	quarantine, err := quarantineNativeRanges(nativeRoot, filepath.ToSlash(relative), filePath, damage)
	if err != nil {
		return repair, false, fmt.Errorf("quarantine damaged native rollout bytes: %w", err)
	}
	repair.QuarantinePath = quarantine
	if err := rewriteNativeRollout(ctx, filePath, before, damage); err != nil {
		// Rename may already have committed before the directory sync failed.
		// Keep the quarantined bytes even when replacement is unconfirmed.
		return repair, false, err
	}
	return repair, true, nil
}

func quarantineNativeRanges(nativeRoot string, relative string, filePath string, damage nativeDamage) (string, error) {
	if len(damage.ranges) == 0 {
		return "", nil
	}
	sum := sha256.Sum256([]byte(relative))
	directory := filepath.Join(nativeRoot, nativeQuarantineDirectory,
		fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:])[:16], time.Now().UnixNano()))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	source, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	manifest := nativeQuarantineManifest{
		Version: 1, RelativePath: relative, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		OriginalSize: damage.size, Reason: "invalid UTF-8 or JSON record in an active native rollout",
	}
	for index, span := range damage.ranges {
		name := fmt.Sprintf("range-%03d.bin", index)
		hasher := sha256.New()
		if err := copyNativeRangeToFile(source, span, filepath.Join(directory, name), hasher); err != nil {
			_ = os.RemoveAll(directory)
			return "", err
		}
		manifest.Ranges = append(manifest.Ranges, nativeQuarantinedRange{
			nativeByteRange: span, SHA256: hex.EncodeToString(hasher.Sum(nil)), File: name,
		})
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	if err := writeNativeFileDurably(filepath.Join(directory, "manifest.json"), data); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	if err := syncDirectory(directory); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	return directory, nil
}

func copyNativeRangeToFile(source *os.File, span nativeByteRange, destination string, hasher io.Writer) error {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(io.MultiWriter(output, hasher), io.NewSectionReader(source, span.Start, span.End-span.Start))
	syncErr := output.Sync()
	closeErr := output.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

func writeNativeFileDurably(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

// rewriteNativeRollout builds the repaired file next to the original and
// renames it into place. The clone shares unchanged blocks on APFS, so a
// damaged region near the end of a very large rollout costs only the rewritten
// tail in disk space.
func rewriteNativeRollout(ctx context.Context, filePath string, original os.FileInfo, damage nativeDamage) error {
	directory := filepath.Dir(filePath)
	temporary := filepath.Join(directory, fmt.Sprintf("%s%d%s", nativeRepairTempPrefix, time.Now().UnixNano(), nativeRepairTempSuffix))
	if err := cloneOrCopyNativeFile(filePath, temporary); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("prepare native rollout repair: %w", err)
	}
	cleanup := func(cause error) error {
		_ = os.Remove(temporary)
		return cause
	}
	source, err := openNativeBacking(filePath, os.O_RDONLY, 0)
	if err != nil {
		return cleanup(err)
	}
	defer source.Close()
	target, err := os.OpenFile(temporary, os.O_WRONLY, 0)
	if err != nil {
		return cleanup(err)
	}

	// Everything before the first damaged range is already identical in the
	// clone, so only the records that follow each damaged range move down.
	write := damage.size
	var keep []nativeByteRange
	if len(damage.ranges) != 0 {
		write = damage.ranges[0].Start
		for index, span := range damage.ranges {
			end := damage.size
			if index+1 < len(damage.ranges) {
				end = damage.ranges[index+1].Start
			}
			if span.End < end {
				keep = append(keep, nativeByteRange{Start: span.End, End: end})
			}
		}
	}
	buffer := make([]byte, nativeRepairCopyChunk)
	for _, span := range keep {
		for position := span.Start; position < span.End; {
			if err := ctx.Err(); err != nil {
				_ = target.Close()
				return cleanup(err)
			}
			count := int(min(int64(len(buffer)), span.End-position))
			if _, err := source.ReadAt(buffer[:count], position); err != nil && !errors.Is(err, io.EOF) {
				_ = target.Close()
				return cleanup(err)
			}
			if _, err := target.WriteAt(buffer[:count], write); err != nil {
				_ = target.Close()
				return cleanup(err)
			}
			position += int64(count)
			write += int64(count)
		}
	}
	if damage.finalNewline {
		if _, err := target.WriteAt([]byte{'\n'}, write); err != nil {
			_ = target.Close()
			return cleanup(err)
		}
		write++
	}
	if err := target.Truncate(write); err != nil {
		_ = target.Close()
		return cleanup(err)
	}
	if err := target.Chmod(original.Mode().Perm()); err != nil {
		_ = target.Close()
		return cleanup(err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return cleanup(err)
	}
	if err := target.Close(); err != nil {
		return cleanup(err)
	}
	if err := os.Chtimes(temporary, original.ModTime(), original.ModTime()); err != nil {
		return cleanup(err)
	}
	after, err := os.Lstat(filePath)
	if err != nil {
		return cleanup(err)
	}
	if !os.SameFile(original, after) || after.Size() != original.Size() || !after.ModTime().Equal(original.ModTime()) {
		return cleanup(fmt.Errorf("native rollout changed during repair: %s", filePath))
	}
	parent, err := os.OpenRoot(directory)
	if err != nil {
		return cleanup(err)
	}
	defer parent.Close()
	// Root.Rename uses handle-relative POSIX replacement on Windows as well,
	// so readers that allowed delete sharing retain the old file contents.
	if err := parent.Rename(filepath.Base(temporary), filepath.Base(filePath)); err != nil {
		return cleanup(err)
	}
	return syncDirectory(directory)
}

func cloneOrCopyNativeFile(source string, destination string) error {
	if err := cloneNativeFile(source, destination); err == nil {
		return nil
	}
	_ = os.Remove(destination)
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func isNativeRepairTemporary(name string) bool {
	return strings.HasPrefix(name, nativeRepairTempPrefix) && strings.HasSuffix(name, nativeRepairTempSuffix)
}
