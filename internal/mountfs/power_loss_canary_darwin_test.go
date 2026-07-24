//go:build darwin

package mountfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	powerLossCanaryModeEnv = "CODEXFOLD_POWER_LOSS_CANARY_MODE"
	powerLossCanaryRootEnv = "CODEXFOLD_POWER_LOSS_CANARY_ROOT"
)

type powerLossCanaryState struct {
	Version           int       `json:"version"`
	RunID             string    `json:"run_id"`
	Phase             string    `json:"phase"`
	Root              string    `json:"root"`
	NativeRoot        string    `json:"native_root"`
	TargetPath        string    `json:"target_path"`
	JournalRoot       string    `json:"journal_root"`
	BaseSize          int64     `json:"base_size"`
	PartialSize       int64     `json:"partial_size,omitempty"`
	FinalSize         int64     `json:"final_size"`
	BaseSHA256        string    `json:"base_sha256"`
	TailSHA256        string    `json:"tail_sha256"`
	FinalSHA256       string    `json:"final_sha256"`
	BootBefore        string    `json:"boot_before"`
	BootAfter         string    `json:"boot_after,omitempty"`
	ArmedAt           time.Time `json:"armed_at"`
	VerifiedAt        time.Time `json:"verified_at,omitempty"`
	ArmPID            int       `json:"arm_pid"`
	VerifyPID         int       `json:"verify_pid,omitempty"`
	PreRecoverySize   int64     `json:"pre_recovery_size,omitempty"`
	PreRecoverySHA256 string    `json:"pre_recovery_sha256,omitempty"`
	RecoveredSize     int64     `json:"recovered_size,omitempty"`
	RecoveredSHA256   string    `json:"recovered_sha256,omitempty"`
	JournalCount      int       `json:"journal_count"`
	Result            string    `json:"result,omitempty"`
	Error             string    `json:"error,omitempty"`
}

func TestPowerLossCanary(t *testing.T) {
	mode := os.Getenv(powerLossCanaryModeEnv)
	root := os.Getenv(powerLossCanaryRootEnv)
	if mode == "" && root == "" {
		t.Skip("set CODEXFOLD_POWER_LOSS_CANARY_MODE and CODEXFOLD_POWER_LOSS_CANARY_ROOT explicitly")
	}
	if root == "" || !filepath.IsAbs(root) {
		t.Fatal("power-loss canary requires an absolute root")
	}
	root = filepath.Clean(root)

	switch mode {
	case "arm":
		if err := armPowerLossCanary(root); err != nil {
			t.Fatal(err)
		}
		t.Fatal("power-loss canary arm returned without a host restart")
	case "verify":
		if err := verifyPowerLossCanary(root); err != nil {
			_ = writePowerLossFailure(root, err)
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported power-loss canary mode %q", mode)
	}
}

func armPowerLossCanary(root string) error {
	if _, err := os.Stat(filepath.Join(root, "READY.json")); err == nil {
		return errors.New("power-loss canary is already armed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	nativeRoot := filepath.Join(root, "native")
	targetPath := filepath.Join(nativeRoot, "sessions", "power-loss", "rollout-canary.jsonl")
	journalRoot := filepath.Join(nativeRoot, ".codexfold-native-journal")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	base := powerLossJSONL("base", 1<<20, 'b')
	tail := powerLossJSONL("tail", 2<<20, 't')
	if err := writeSyncedFile(targetPath, base, 0o600); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return err
	}
	bootBefore, err := powerLossBootID()
	if err != nil {
		return err
	}
	state := powerLossCanaryState{
		Version: 1, RunID: filepath.Base(root), Phase: "arming", Root: root,
		NativeRoot: nativeRoot, TargetPath: targetPath, JournalRoot: journalRoot,
		BaseSize: int64(len(base)), FinalSize: int64(len(base) + len(tail)),
		BaseSHA256: digestBytes(base), TailSHA256: digestBytes(tail),
		FinalSHA256: digestBytes(append(append([]byte(nil), base...), tail...)),
		BootBefore:  bootBefore, ArmedAt: time.Now(), ArmPID: os.Getpid(),
	}
	if err := writeDurableJSON(filepath.Join(root, "ARMING.json"), state); err != nil {
		return err
	}

	nativeAppendJournalCheckpoint = func(record nativeAppendJournal, committedTail []byte) {
		if record.TargetPath != targetPath || !bytes.Equal(committedTail, tail) {
			panic("power-loss checkpoint received an unexpected transaction")
		}
		partial := committedTail[:len(committedTail)/2]
		file, openErr := os.OpenFile(record.TargetPath, os.O_WRONLY, 0)
		if openErr != nil {
			panic(openErr)
		}
		n, writeErr := file.WriteAt(partial, record.BaseSize)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil || n != len(partial) {
			panic(fmt.Sprintf("write durable partial tail: n=%d write=%v sync=%v close=%v", n, writeErr, syncErr, closeErr))
		}
		entries, readErr := os.ReadDir(journalRoot)
		if readErr != nil || len(entries) != 1 {
			panic(fmt.Sprintf("durable journal not observable: count=%d err=%v", len(entries), readErr))
		}
		state.Phase = "ready-for-power-loss"
		state.PartialSize = record.BaseSize + int64(len(partial))
		state.JournalCount = len(entries)
		if writeErr := writeDurableJSON(filepath.Join(root, "READY.json"), state); writeErr != nil {
			panic(writeErr)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	return commitNativeAppend(targetPath, journalRoot, int64(len(base)), tail)
}

func verifyPowerLossCanary(root string) error {
	passPath := filepath.Join(root, "PASS.json")
	if _, err := os.Stat(passPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(filepath.Join(root, "READY.json"))
	if err != nil {
		return fmt.Errorf("read durable READY state: %w", err)
	}
	var state powerLossCanaryState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode durable READY state: %w", err)
	}
	if err := validatePowerLossState(root, state); err != nil {
		return err
	}
	bootAfter, err := powerLossBootID()
	if err != nil {
		return err
	}
	if bootAfter == state.BootBefore {
		return errors.New("boot identity did not change; refusing to count a process restart as a power-loss test")
	}
	state.BootAfter = bootAfter
	state.VerifyPID = os.Getpid()
	state.Phase = "verifying"

	preInfo, err := os.Stat(state.TargetPath)
	if err != nil {
		return fmt.Errorf("stat pre-recovery target: %w", err)
	}
	state.PreRecoverySize = preInfo.Size()
	state.PreRecoverySHA256, err = digestFile(state.TargetPath)
	if err != nil {
		return err
	}
	if preInfo.Size() != state.PartialSize || preInfo.Size() <= state.BaseSize || preInfo.Size() >= state.FinalSize {
		return fmt.Errorf("target is not at the armed partial boundary: size=%d base=%d partial=%d final=%d", preInfo.Size(), state.BaseSize, state.PartialSize, state.FinalSize)
	}
	entries, err := os.ReadDir(state.JournalRoot)
	if err != nil || len(entries) != 1 {
		return fmt.Errorf("expected one durable journal before recovery: count=%d err=%w", len(entries), err)
	}
	state.JournalCount = len(entries)
	if err := recoverNativeAppendTransactions(state.NativeRoot, state.JournalRoot); err != nil {
		return fmt.Errorf("run product startup recovery: %w", err)
	}

	recovered, err := os.ReadFile(state.TargetPath)
	if err != nil {
		return fmt.Errorf("read recovered target: %w", err)
	}
	state.RecoveredSize = int64(len(recovered))
	state.RecoveredSHA256 = digestBytes(recovered)
	if state.RecoveredSize != state.BaseSize || state.RecoveredSHA256 != state.BaseSHA256 {
		return fmt.Errorf("recovery did not restore the exact base: size=%d/%d sha=%s/%s", state.RecoveredSize, state.BaseSize, state.RecoveredSHA256, state.BaseSHA256)
	}
	if !completePowerLossJSONL(recovered) {
		return errors.New("recovered target is not complete JSONL")
	}
	entries, err = os.ReadDir(state.JournalRoot)
	if err != nil {
		return fmt.Errorf("read recovered journal directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("recovery left %d journal entries", len(entries))
	}
	state.JournalCount = 0
	state.Phase = "passed"
	state.Result = "PASS"
	state.VerifiedAt = time.Now()
	return writeDurableJSON(passPath, state)
}

func validatePowerLossState(root string, state powerLossCanaryState) error {
	if state.Version != 1 || state.Phase != "ready-for-power-loss" || filepath.Clean(state.Root) != root {
		return errors.New("durable READY state metadata is invalid")
	}
	for _, path := range []string{state.NativeRoot, state.TargetPath, state.JournalRoot} {
		relative, err := filepath.Rel(root, filepath.Clean(path))
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("durable READY path is outside the canary root: %s", path)
		}
	}
	if state.BaseSize <= 0 || state.PartialSize <= state.BaseSize || state.PartialSize >= state.FinalSize ||
		len(state.BaseSHA256) != sha256.Size*2 || len(state.TailSHA256) != sha256.Size*2 || len(state.FinalSHA256) != sha256.Size*2 ||
		state.BootBefore == "" {
		return errors.New("durable READY transaction metadata is invalid")
	}
	return nil
}

func powerLossJSONL(kind string, minimumSize int, fill byte) []byte {
	prefix := fmt.Sprintf("{\"kind\":%q,\"payload\":\"", kind)
	suffix := "\"}\n"
	payloadSize := minimumSize - len(prefix) - len(suffix)
	if payloadSize < 1 {
		payloadSize = 1
	}
	return append(append([]byte(prefix), bytes.Repeat([]byte{fill}, payloadSize)...), suffix...)
}

func completePowerLossJSONL(data []byte) bool {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return false
	}
	for _, line := range bytes.Split(data[:len(data)-1], []byte{'\n'}) {
		if len(line) == 0 || !json.Valid(line) {
			return false
		}
	}
	return true
}

func powerLossBootID() (string, error) {
	output, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return "", fmt.Errorf("read boot identity: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func writePowerLossFailure(root string, cause error) error {
	state := powerLossCanaryState{Version: 1, Root: root, Phase: "failed", Result: "FAIL", Error: cause.Error(), VerifiedAt: time.Now(), VerifyPID: os.Getpid()}
	if data, err := os.ReadFile(filepath.Join(root, "READY.json")); err == nil {
		_ = json.Unmarshal(data, &state)
		state.Phase, state.Result, state.Error = "failed", "FAIL", cause.Error()
		state.VerifiedAt, state.VerifyPID = time.Now(), os.Getpid()
	}
	return writeDurableJSON(filepath.Join(root, "FAIL.json"), state)
}

func writeDurableJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".power-loss-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func writeSyncedFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func digestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return digestBytes(data), nil
}
