package mountfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codexzstd"
	"github.com/samekind/codexfold/internal/nativezst"
)

func foldedZstFixture(t *testing.T) (*Filesystem, *nativezst.Source, string, string, []byte) {
	t.Helper()
	if !codexzstd.Supported() {
		t.Skip("folded .zst files need cgo")
	}
	root := t.TempDir()
	store := filepath.Join(root, "store")
	nativeRoot := filepath.Join(root, "native")
	route := "/archived_sessions/rollout-2026-09-01T10-00-00-0199aaaa-0000-7000-8000-00000000000a.jsonl.zst"
	nativePath := nativePathFromRoot(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	// A unique history replayed beyond zstd's window, as a fork does: the
	// store folds it for less than Codex's own .zst.
	var history bytes.Buffer
	for line := 0; history.Len() < 2400<<10; line++ {
		fmt.Fprintf(&history, `{"ordinal":%d,"text":"line %d of a compressed Codex rollout %x"}`+"\n", line, line*7919, sha256.Sum256([]byte(fmt.Sprint(line))))
	}
	var content bytes.Buffer
	content.Write(history.Bytes())
	content.Write(history.Bytes())
	var encoded bytes.Buffer
	if err := codexzstd.Encode(context.Background(), &encoded, bytes.NewReader(content.Bytes()), int64(content.Len())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativePath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	modTime := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(nativePath, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	source := nativezst.NewSource(store, nil)
	t.Cleanup(func() { _ = source.Close() })
	return filesystem, source, store, route, encoded.Bytes()
}

func convertFixture(t *testing.T, filesystem *Filesystem, source *nativezst.Source, store string, route string) {
	t.Helper()
	nativePath := nativePathFromRoot(filesystem.nativeZstRoot(), route)
	prepared, err := nativezst.Prepare(context.Background(), source, nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := filesystem.ConvertNativeZst(store, prepared); err != nil {
		t.Fatal(err)
	}
}

func TestFoldedNativeZstServesOriginalBytes(t *testing.T) {
	filesystem, source, store, route, original := foldedZstFixture(t)
	before, errno := filesystem.Getattr(route)
	if errno != 0 {
		t.Fatal(errno)
	}
	convertFixture(t, filesystem, source, store, route)

	// Without a source the mount must refuse rather than serve zeros.
	if _, errno := filesystem.Open(route, os.O_RDONLY); errno != syscall.EIO {
		t.Fatalf("open without source = %v, want EIO", errno)
	}
	filesystem.SetNativeZstSource(source)

	after, errno := filesystem.Getattr(route)
	if errno != 0 {
		t.Fatal(errno)
	}
	if after.Size != before.Size || !after.ModTime.Equal(before.ModTime) || after.Mode != before.Mode || after.ObjectID != before.ObjectID {
		t.Fatalf("attributes changed: before=%+v after=%+v", before, after)
	}
	handle, errno := filesystem.Open(route, os.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	var got bytes.Buffer
	buffer := make([]byte, 4096)
	for offset := int64(0); ; {
		n, errno := filesystem.Read(handle, buffer, offset)
		if errno != 0 {
			t.Fatal(errno)
		}
		if n == 0 {
			break
		}
		got.Write(buffer[:n])
		offset += int64(n)
	}
	if !bytes.Equal(got.Bytes(), original) {
		t.Fatal("mount served different bytes")
	}
	var streamed bytes.Buffer
	if _, err := filesystem.StreamBufferedRead(handle, 0, len(original)+10, 1000, func(total int, chunk []byte) error {
		if total != len(original) {
			return fmt.Errorf("total = %d", total)
		}
		streamed.Write(chunk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamed.Bytes(), original) {
		t.Fatal("buffered stream served different bytes")
	}
	if handled, _, _ := filesystem.StreamNativeRead(handle, 0, 10, func(*os.File, int64, int) (int, error) { return 0, errors.New("unexpected") }); handled {
		t.Fatal("folded file exposed its native descriptor")
	}
	if handled, _ := filesystem.WithNativeReadFD(handle, func(*os.File) error { return errors.New("unexpected") }); handled {
		t.Fatal("folded file transferred its native descriptor")
	}

	// A conversion never runs under an open handle.
	nativePath := nativePathFromRoot(filesystem.nativeZstRoot(), route)
	marker, _, err := nativezst.ReadMarker(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := filesystem.FinishNativeZst(context.Background(), source, nativezst.NativeFile{Path: nativePath, Info: info, Marker: marker, Folded: true}); !errors.Is(err, ErrNativeZstBusy) {
		t.Fatalf("finish under an open handle = %v", err)
	}
	if errno := filesystem.Release(handle); errno != 0 {
		t.Fatal(errno)
	}
}

func TestFoldedNativeZstIsReadOnlyAndHidesItsMarker(t *testing.T) {
	filesystem, source, store, route, _ := foldedZstFixture(t)
	convertFixture(t, filesystem, source, store, route)
	filesystem.SetNativeZstSource(source)
	for _, flags := range []int{os.O_WRONLY, os.O_RDWR, os.O_RDONLY | os.O_TRUNC} {
		if _, errno := filesystem.Open(route, flags); errno != syscall.EPERM {
			t.Fatalf("open flags %#x = %v, want EPERM", flags, errno)
		}
	}
	if errno := filesystem.TruncatePath(route, 0); errno != syscall.EPERM {
		t.Fatalf("truncate = %v, want EPERM", errno)
	}
	if _, errno := filesystem.GetXattr(route, nativezst.MarkerAttribute); errno != xattrMissingErrno() {
		t.Fatalf("marker read through the mount = %v", errno)
	}
	attributes, errno := filesystem.ListXattrs(route)
	if errno != 0 || slices.Contains(attributes, nativezst.MarkerAttribute) {
		t.Fatalf("listxattr = %v %v", attributes, errno)
	}
	if errno := filesystem.SetXattr(route, nativezst.MarkerAttribute, []byte("{}"), 0); errno != syscall.EPERM {
		t.Fatalf("marker overwrite = %v, want EPERM", errno)
	}
}

func TestConvertNativeZstRefusesAnOpenFile(t *testing.T) {
	filesystem, source, store, route, _ := foldedZstFixture(t)
	filesystem.SetNativeZstSource(source)
	handle, errno := filesystem.Open(route, os.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	nativePath := nativePathFromRoot(filesystem.nativeZstRoot(), route)
	prepared, err := nativezst.Prepare(context.Background(), source, nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := filesystem.ConvertNativeZst(store, prepared); !errors.Is(err, ErrNativeZstBusy) {
		t.Fatalf("convert under an open handle = %v", err)
	}
	if _, folded, _ := nativezst.ReadMarker(nativePath); folded {
		t.Fatal("busy file was converted")
	}
	filesystem.Release(handle)
}

func TestScanNativeZstFindsFoldedFiles(t *testing.T) {
	filesystem, source, store, route, _ := foldedZstFixture(t)
	convertFixture(t, filesystem, source, store, route)
	scan, err := filesystem.ScanNativeZst(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Folded) != 1 || len(scan.Candidates) != 0 {
		t.Fatalf("scan folded=%d candidates=%d", len(scan.Folded), len(scan.Candidates))
	}
}
