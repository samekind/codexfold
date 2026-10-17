package vfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/samekind/codexfold/internal/storage"
)

func TestAppendNoSpaceRollsBackPartialRecordAndCanRetry(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	before := []byte("{\"accepted\":true}\n")
	if _, err := writer.Append(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	failed := []byte("{\"retry\":true}\n")
	writer.appendWrite = func(f *os.File, p []byte) (int, error) {
		n, err := f.Write(p[:len(p)/2])
		if err != nil {
			return n, err
		}
		return n, syscall.ENOSPC
	}
	if n, err := writer.Append(context.Background(), failed); n != 0 || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("partial ENOSPC: n=%d err=%v", n, err)
	}
	got, err := os.ReadFile(session.State().DeltaPath)
	if err != nil || !bytes.Equal(got, before) {
		t.Fatalf("accepted bytes changed or partial record survived: %q %v", got, err)
	}
	writer.appendWrite = nil
	if n, err := writer.Append(context.Background(), failed); n != len(failed) || err != nil {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	r, err := session.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	want := append(append(append([]byte{}, source...), before...), failed...)
	if got := readHandle(t, r); !bytes.Equal(got, want) {
		t.Fatal("read after space recovery differs")
	}
}

func TestAppendActualFullVolumePreservesReadsAndRecovers(t *testing.T) {
	volume := os.Getenv("CODEXFOLD_TEST_FULL_VOLUME")
	if !strings.HasPrefix(volume, "/private/tmp/codexfold-pressure-") {
		t.Skip("requires an explicitly created small test volume")
	}
	space, err := storage.AvailableBytes(volume)
	if err != nil || space > 300<<20 {
		t.Fatalf("not a small disposable volume: space=%d err=%v", space, err)
	}
	root, err := os.MkdirTemp(volume, "vfs-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	r, err := session.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	filler := filepath.Join(root, "test-only-filler")
	f, err := os.Create(filler)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 1<<20)
	var fillErr error
	for i := 0; i < 300; i++ {
		if _, fillErr = f.Write(block); fillErr != nil {
			break
		}
	}
	_ = f.Close()
	if !errors.Is(fillErr, syscall.ENOSPC) {
		t.Fatalf("did not reach actual ENOSPC: %v", fillErr)
	}
	if got := readHandle(t, r); !bytes.Equal(got, source) {
		t.Fatal("existing read changed at ENOSPC")
	}
	if n, err := writer.Append(context.Background(), block); n != 0 || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("full-volume append n=%d err=%v", n, err)
	}
	if got, err := os.ReadFile(session.State().DeltaPath); err != nil || len(got) != 0 {
		t.Fatalf("partial append remained: bytes=%d err=%v", len(got), err)
	}
	_ = r.Close()
	r, err = session.OpenReader()
	if err != nil {
		t.Fatalf("fresh reader at ENOSPC: %v", err)
	}
	if got := readHandle(t, r); !bytes.Equal(got, source) {
		t.Fatal("fresh reader differs on full volume")
	}
	_ = r.Close()
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	appendix := []byte("{\"space_recovered\":true}\n")
	if n, err := writer.Append(context.Background(), appendix); n != len(appendix) || err != nil {
		t.Fatalf("automatic retry n=%d err=%v", n, err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openFixtureSession(t, root, manifest, reader, nil)
	check, err := reopened.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if got := readHandle(t, check); !bytes.Equal(got, append(append([]byte{}, source...), appendix...)) {
		t.Fatal("reopen after freeing space lost bytes")
	}
	t.Log("actual ENOSPC: existing/fresh reads, failed append rollback, automatic retry and reopen passed")
}
