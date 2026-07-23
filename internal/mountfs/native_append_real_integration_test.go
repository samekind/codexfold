//go:build darwin && fuse && cgo

package mountfs

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

const (
	nativeAppendCrashHelperEnv = "CODEXFOLD_NATIVE_APPEND_CRASH_HELPER"
	nativeAppendCrashRootEnv   = "CODEXFOLD_NATIVE_APPEND_CRASH_ROOT"
	realCodexTraceBaseBytes    = 51836
	realCodexTraceFinalBytes   = 57255
)

type codexWriteReplayOperation struct {
	kind   string
	offset int64
	size   int
	flags  int
}

func TestRealFuseReplaysSanitizedCodexWriteTraceAgainstAPFS(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	route := filepath.Join("sessions", "2026", "07", "16", "rollout-trace-replay.jsonl")
	nativeRoot := filepath.Join(root, "native")
	nativePath := filepath.Join(nativeRoot, route)
	referencePath := filepath.Join(root, "apfs-reference.jsonl")
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}

	base := exactJSONLRecord(t, realCodexTraceBaseBytes, 'b')
	tail := exactJSONLRecord(t, realCodexTraceFinalBytes-realCodexTraceBaseBytes, 't')
	want := append(append([]byte(nil), base...), tail...)
	for _, target := range []string{referencePath, nativePath} {
		if err := os.WriteFile(target, base, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	operations := loadCodexWriteReplay(t, filepath.Join("testdata", "codex-real-resume-write.trace"))

	var recordedMu sync.Mutex
	var recorded []string
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	stopMount := startRealMountWithOptions(t, HostOptions{
		MountPoint: mountPoint,
		Filesystem: filesystem,
		Foreground: true,
		OperationRecorder: func(operation string) {
			recordedMu.Lock()
			defer recordedMu.Unlock()
			recorded = append(recorded, operation)
		},
	})
	target := filepath.Join(mountPoint, route)
	waitForRealFile(t, target, base)

	replayCodexWriteOperations(t, referencePath, want, operations)
	replayCodexWriteOperations(t, target, want, operations)
	assertNativeBytes(t, nativePath, want)
	assertNativeBytes(t, referencePath, want)
	visible, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(visible, want) {
		t.Fatalf("mounted replay bytes=%d err=%v want=%d", len(visible), err, len(want))
	}
	if !completeJSONL(want) {
		t.Fatal("trace replay fixture did not produce valid JSONL")
	}

	recordedMu.Lock()
	joined := strings.Join(recorded, "\n")
	recordedMu.Unlock()
	for _, operation := range operations {
		if operation.kind != "write" {
			continue
		}
		marker := fmt.Sprintf("offset=%d bytes=%d result=%d", operation.offset, operation.size, operation.size)
		if !strings.Contains(joined, marker) {
			t.Fatalf("real FUSE trace did not replay %q: %s", marker, joined)
		}
	}
	for _, marker := range []string{"open kind=session flags=0x2", "fsync kind=session", "flush kind=session", "release kind=session"} {
		if !strings.Contains(joined, marker) {
			t.Fatalf("real FUSE replay trace missing %q: %s", marker, joined)
		}
	}
	for _, entry := range strings.Split(joined, "\n") {
		if strings.Contains(entry, "kind=session") && strings.Contains(entry, "result=-") {
			t.Fatalf("real FUSE replay operation failed: %s", entry)
		}
	}
	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func TestRealFuseNativeAppendSIGKILLRecovery(t *testing.T) {
	if os.Getenv(nativeAppendCrashHelperEnv) == "1" {
		runNativeAppendCrashHelper(t)
		return
	}
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := filepath.Join("sessions", "2026", "07", "16", "rollout-sigkill-recovery.jsonl")
	nativePath := filepath.Join(nativeRoot, route)
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":\"before-crash\"}\n")
	crashTail := exactJSONLRecord(t, 128<<10, 'c')
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestRealFuseNativeAppendSIGKILLRecovery$", "-test.v")
	command.Env = append(os.Environ(), nativeAppendCrashHelperEnv+"=1", nativeAppendCrashRootEnv+"="+root)
	helperLogPath := filepath.Join(root, "crash-helper.log")
	helperLog, err := os.Create(helperLogPath)
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = helperLog, helperLog
	runErr := command.Run()
	closeErr := helperLog.Close()
	output, readErr := os.ReadFile(helperLogPath)
	if closeErr != nil || readErr != nil {
		t.Fatalf("read crash helper log: close=%v read=%v", closeErr, readErr)
	}
	if runErr == nil {
		t.Fatalf("crash helper exited normally: %s", output)
	}
	exitError, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("crash helper did not return an exit status: %v: %s", runErr, output)
	}
	waitStatus, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok || !waitStatus.Signaled() || waitStatus.Signal() != syscall.SIGKILL {
		t.Fatalf("crash helper was not SIGKILLed: status=%v err=%v: %s", exitError.Sys(), runErr, output)
	}

	info, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= int64(len(base)) || info.Size() >= int64(len(base)+len(crashTail)) {
		t.Fatalf("crash did not leave a partial backing write: size=%d base=%d final=%d", info.Size(), len(base), len(base)+len(crashTail))
	}
	journalRoot := filepath.Join(nativeRoot, ".codexfold-native-journal")
	entries, err := os.ReadDir(journalRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("durable recovery journal missing after SIGKILL: entries=%d err=%v", len(entries), err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	if err := filesystem.RecoverNativeAppendTransactions(); err != nil {
		t.Fatalf("recover SIGKILL transaction: %v", err)
	}
	assertNativeBytes(t, nativePath, base)
	assertEmptyJournal(t, journalRoot)

	stopMount := startRealMount(t, mountPoint, filesystem)
	target := filepath.Join(mountPoint, route)
	waitForRealFile(t, target, base)
	afterRecovery := []byte("{\"record\":\"after-recovery\"}\n")
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, writeErr := file.Write(afterRecovery); writeErr != nil || n != len(afterRecovery) {
		_ = file.Close()
		t.Fatalf("append after SIGKILL recovery: n=%d err=%v", n, writeErr)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close after SIGKILL recovery: %v", err)
	}
	want := append(append([]byte(nil), base...), afterRecovery...)
	assertNativeBytes(t, nativePath, want)
	if !completeJSONL(want) {
		t.Fatal("post-recovery append produced invalid JSONL")
	}
	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func runNativeAppendCrashHelper(t *testing.T) {
	root := os.Getenv(nativeAppendCrashRootEnv)
	if root == "" {
		t.Fatal("crash helper root is missing")
	}
	nativeRoot := filepath.Join(root, "native")
	nativePath := filepath.Join(nativeRoot, "sessions", "2026", "07", "16", "rollout-sigkill-recovery.jsonl")
	journalRoot := filepath.Join(nativeRoot, ".codexfold-native-journal")
	tail := exactJSONLRecord(t, 128<<10, 'c')
	nativeAppendJournalCheckpoint = func(record nativeAppendJournal, committedTail []byte) {
		if !bytes.Equal(committedTail, tail) {
			panic("checkpoint received an unexpected append tail")
		}
		file, err := os.OpenFile(record.TargetPath, os.O_WRONLY, 0)
		if err != nil {
			panic(err)
		}
		partial := committedTail[:len(committedTail)/2]
		if n, err := file.WriteAt(partial, record.BaseSize); err != nil || n != len(partial) {
			panic(fmt.Sprintf("partial crash write n=%d err=%v", n, err))
		}
		if err := file.Sync(); err != nil {
			panic(err)
		}
		if err := file.Close(); err != nil {
			panic(err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			panic(err)
		}
		select {}
	}
	info, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitNativeAppend(nativePath, journalRoot, info.Size(), tail); err != nil {
		t.Fatalf("crash checkpoint was not reached: %v", err)
	}
	t.Fatal("crash checkpoint was not reached")
}

func loadCodexWriteReplay(t *testing.T, path string) []codexWriteReplayOperation {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var operations []codexWriteReplayOperation
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		operation := codexWriteReplayOperation{kind: fields[0]}
		switch operation.kind {
		case "open":
			if len(fields) != 2 {
				t.Fatalf("invalid open replay operation: %q", line)
			}
			flags, err := strconv.ParseInt(fields[1], 0, 32)
			if err != nil {
				t.Fatal(err)
			}
			operation.flags = int(flags)
		case "write":
			if len(fields) != 3 {
				t.Fatalf("invalid write replay operation: %q", line)
			}
			offset, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			size, err := strconv.Atoi(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			operation.offset, operation.size = offset, size
		case "fsync", "flush", "release":
			if len(fields) != 1 {
				t.Fatalf("invalid %s replay operation: %q", operation.kind, line)
			}
		default:
			t.Fatalf("unknown replay operation: %q", line)
		}
		operations = append(operations, operation)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return operations
}

func replayCodexWriteOperations(t *testing.T, path string, final []byte, operations []codexWriteReplayOperation) {
	t.Helper()
	var file *os.File
	for _, operation := range operations {
		switch operation.kind {
		case "open":
			if file != nil {
				t.Fatal("replay opened an already-open file")
			}
			var err error
			file, err = os.OpenFile(path, operation.flags, 0)
			if err != nil {
				t.Fatal(err)
			}
		case "write":
			if file == nil || operation.offset < 0 || operation.size < 0 || operation.offset+int64(operation.size) > int64(len(final)) {
				t.Fatalf("invalid replay write: offset=%d size=%d final=%d", operation.offset, operation.size, len(final))
			}
			chunk := final[operation.offset : operation.offset+int64(operation.size)]
			if n, err := file.WriteAt(chunk, operation.offset); err != nil || n != len(chunk) {
				t.Fatalf("replay write offset=%d size=%d: n=%d err=%v", operation.offset, operation.size, n, err)
			}
		case "fsync":
			if file == nil {
				t.Fatal("replay fsync without an open file")
			}
			if err := file.Sync(); err != nil {
				t.Fatal(err)
			}
		case "flush":
			// FUSE emits flush during close; there is no separate portable os.File call.
		case "release":
			if file == nil {
				t.Fatal("replay release without an open file")
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			file = nil
		}
	}
	if file != nil {
		_ = file.Close()
		t.Fatal("replay trace ended with an open file")
	}
}

func exactJSONLRecord(t *testing.T, size int, fill byte) []byte {
	t.Helper()
	prefix := []byte("{\"payload\":\"")
	suffix := []byte("\"}\n")
	if size < len(prefix)+len(suffix) {
		t.Fatalf("JSONL record size %d is too small", size)
	}
	record := append([]byte(nil), prefix...)
	record = append(record, bytes.Repeat([]byte{fill}, size-len(prefix)-len(suffix))...)
	record = append(record, suffix...)
	if len(record) != size || !completeJSONL(record) {
		t.Fatalf("invalid exact JSONL record: size=%d want=%d", len(record), size)
	}
	return record
}
