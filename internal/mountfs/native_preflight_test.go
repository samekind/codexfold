package mountfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeWriterPreflightValidatesOnlyActiveRollouts(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "sessions", "2026", "07", "16")
	archived := filepath.Join(root, "archived_sessions")
	if err := os.MkdirAll(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archived, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := []byte("{\"record\":0}\n{\"record\":1}\n")
	if err := os.WriteFile(filepath.Join(active, "rollout-valid.jsonl"), valid, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "._rollout-valid.jsonl"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archived, "rollout-old.jsonl"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := validateNativeWriterRollouts(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 1 || report.Bytes != int64(len(valid)) {
		t.Fatalf("preflight report = %#v", report)
	}
}

func TestNativeWriterPreflightRejectsInvalidJSON(t *testing.T) {
	root, _ := nativePreflightFixture(t, []byte("{\"record\":0}\nnot-json\n"))
	_, err := validateNativeWriterRollouts(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("invalid JSON preflight error = %v", err)
	}
}

func TestNativeWriterPreflightRejectsInvalidUTF8(t *testing.T) {
	root, _ := nativePreflightFixture(t, []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n'})
	_, err := validateNativeWriterRollouts(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("invalid UTF-8 preflight error = %v", err)
	}
}

func TestNativeWriterPreflightRejectsMissingFinalNewline(t *testing.T) {
	root, _ := nativePreflightFixture(t, []byte("{\"record\":0}"))
	_, err := validateNativeWriterRollouts(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "missing its final newline") {
		t.Fatalf("missing newline preflight error = %v", err)
	}
}

func TestValidateNativeRolloutRejectsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.jsonl")
	if err := os.WriteFile(target, []byte("{\"record\":0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateNativeRollout(context.Background(), link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink validation error = %v", err)
	}
}

func TestNativeWriterPreflightRejectsSymlink(t *testing.T) {
	root, path := nativePreflightFixture(t, []byte("{\"record\":0}\n"))
	link := filepath.Join(filepath.Dir(path), "rollout-link.jsonl")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, err := validateNativeWriterRollouts(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "rejects symlink") {
		t.Fatalf("symlink preflight error = %v", err)
	}
}

func TestNativeWriterPreflightHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := validateNativeWriterRollouts(ctx, root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preflight error = %v", err)
	}
}

func TestNativeWriterPreflightCachesAndValidatesOnlyNewTail(t *testing.T) {
	root, path := nativePreflightFixture(t, []byte("{\"record\":0}\n"))
	first, err := validateNativeWriterRollouts(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if first.ValidatedFiles != 1 || first.CachedFiles != 0 || first.IncrementalFiles != 0 {
		t.Fatalf("first preflight = %#v", first)
	}
	second, err := validateNativeWriterRollouts(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if second.CachedFiles != 1 || second.ValidatedFiles != 0 || second.ValidatedBytes != 0 {
		t.Fatalf("cached preflight = %#v", second)
	}

	tail := []byte("{\"record\":1}\n")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(tail); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	incremental, err := validateNativeWriterRollouts(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if incremental.IncrementalFiles != 1 || incremental.ValidatedFiles != 0 || incremental.ValidatedBytes != int64(len(tail)) {
		t.Fatalf("incremental preflight = %#v", incremental)
	}
}

func TestNativeWriterPreflightFullyRevalidatesSameSizeMutation(t *testing.T) {
	root, path := nativePreflightFixture(t, []byte("{\"record\":0}\n"))
	if _, err := validateNativeWriterRollouts(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("X"), 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := validateNativeWriterRollouts(context.Background(), root); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("same-size mutation preflight error = %v", err)
	}
}

func TestNativeWriterPreflightRebuildsCorruptCache(t *testing.T) {
	root, _ := nativePreflightFixture(t, []byte("{\"record\":0}\n"))
	cachePath := filepath.Join(root, ".codexfold-native-preflight-v1.json")
	if err := os.WriteFile(cachePath, []byte("broken-cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := validateNativeWriterRollouts(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !report.CacheRebuilt || report.ValidatedFiles != 1 {
		t.Fatalf("rebuilt preflight = %#v", report)
	}
	if _, _, rebuilt := loadNativePreflightCache(cachePath); rebuilt {
		t.Fatal("rewritten preflight cache is still invalid")
	}
}

func TestNativeWriterPreflightExternalRoot(t *testing.T) {
	root := os.Getenv("CODEXFOLD_NATIVE_PREFLIGHT_ROOT")
	if root == "" {
		t.Skip("set CODEXFOLD_NATIVE_PREFLIGHT_ROOT to scan an external native root")
	}
	report, err := validateNativeWriterRollouts(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("validated active native rollouts: files=%d bytes=%d", report.Files, report.Bytes)
}

func nativePreflightFixture(t *testing.T, data []byte) (string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "2026", "07", "16", "rollout-fixture.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}
