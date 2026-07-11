//go:build darwin && fuse && cgo

package mountfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/service"
	"github.com/jstar0/codexfold/internal/vfs"
)

func TestRealFuseMountNativeFileOperations(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real macFUSE adapter test")
	}
	root := t.TempDir()
	source := []byte("first\nsecond\nthird\n")
	digest := sha256.Sum256(source)
	digestHex := hex.EncodeToString(digest[:])
	nativePath := filepath.Join(root, "native.jsonl")
	if err := os.WriteFile(nativePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := fold.Manifest{
		Version: fold.ManifestVersion, Kind: fold.ManifestKind,
		Session: fold.ManifestSession{ID: "fixture", RolloutPath: nativePath, Archived: true},
		Source:  fold.ManifestSource{Bytes: int64(len(source)), SHA256: digestHex},
		Parts:   []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: digestHex, RawBytes: int64(len(source))}}},
	}
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest,
		Reader: fuseFixtureReader{digestHex: source}, NativeSnapshot: vfs.NativeFile{Path: nativePath, Bytes: int64(len(source)), SHA256: digestHex},
	})
	if err != nil {
		t.Fatal(err)
	}
	filesystem := New()
	if err := filesystem.AddSession("fixture", managed); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealMount(t, mountPoint, filesystem)
	target := filepath.Join(mountPoint, "fixture.jsonl")
	entries, err := os.ReadDir(mountPoint)
	if err != nil || len(entries) != 1 || entries[0].Name() != "fixture.jsonl" {
		t.Fatalf("native directory listing = %#v err=%v", entries, err)
	}
	initialInfo, err := os.Stat(target)
	if err != nil || initialInfo.Size() != int64(len(source)) || initialInfo.Mode().Perm() != 0o600 {
		t.Fatalf("initial native stat = %#v err=%v", initialInfo, err)
	}
	read, err := os.ReadFile(target)
	if err != nil || string(read) != string(source) {
		t.Fatalf("native read differs: %q err=%v", read, err)
	}
	appendFile, err := os.OpenFile(target, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	tail := []byte("tail\n")
	if _, err := appendFile.Write(tail); err != nil {
		_ = appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Sync(); err != nil {
		_ = appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}
	read, err = os.ReadFile(target)
	want := append(append([]byte(nil), source...), tail...)
	if err != nil || string(read) != string(want) {
		t.Fatalf("append read differs: %q err=%v", read, err)
	}
	randomFile, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := randomFile.WriteAt([]byte("PATCH"), 2); err != nil {
		_ = randomFile.Close()
		t.Fatal(err)
	}
	if err := randomFile.Truncate(int64(len(want) - 3)); err != nil {
		_ = randomFile.Close()
		t.Fatal(err)
	}
	if err := randomFile.Sync(); err != nil {
		_ = randomFile.Close()
		t.Fatal(err)
	}
	if err := randomFile.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() != int64(len(want)-3) {
		t.Fatalf("stat after truncate: %#v err=%v", info, err)
	}
	if err := os.Rename(target, filepath.Join(mountPoint, "renamed.jsonl")); err == nil {
		t.Fatal("rename should fail closed until a real Codex trace requires it")
	}
	mutated := append([]byte(nil), want...)
	copy(mutated[2:], []byte("PATCH"))
	mutated = mutated[:len(mutated)-3]
	read, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(read, mutated) {
		t.Fatalf("random-write/truncate read differs: %q err=%v", read, err)
	}
	stopMount()
	waitForRealUnmount(t, mountPoint)

	stopRemount := startRealMount(t, mountPoint, filesystem)
	read, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(read, mutated) {
		t.Fatalf("remount read differs: %q err=%v", read, err)
	}
	stopRemount()
	waitForRealUnmount(t, mountPoint)
}

func startRealMount(t *testing.T, mountPoint string, filesystem *Filesystem) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	mountDone := make(chan error, 1)
	go func() {
		mountDone <- Mount(ctx, HostOptions{MountPoint: mountPoint, Filesystem: filesystem, Foreground: true})
	}()
	var stopOnce sync.Once
	stopMount := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-mountDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("mount shutdown: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("mount did not stop after cancellation")
			}
		})
	}
	t.Cleanup(stopMount)
	waitForRealMount(t, mountPoint, mountDone)
	return stopMount
}

func waitForRealMount(t *testing.T, mountPoint string, mountDone <-chan error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := service.ProbeMount(mountPoint); err == nil {
			return
		}
		select {
		case err := <-mountDone:
			t.Fatalf("mount exited before becoming healthy: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("FUSE mount did not become healthy")
}

func waitForRealUnmount(t *testing.T, mountPoint string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := service.ProbeMount(mountPoint); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("FUSE mount remained active after shutdown")
}

type fuseFixtureReader map[string][]byte

func (r fuseFixtureReader) ReadAt(_ context.Context, ref fold.ObjectRef, destination []byte, offset int64) (int, error) {
	data := r[ref.SHA256]
	if offset >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(destination, data[offset:])
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}
