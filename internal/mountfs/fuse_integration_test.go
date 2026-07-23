//go:build darwin && fuse && cgo

package mountfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/service"
	"github.com/samekind/codexfold/internal/vfs"
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/unix"
)

func TestOperationTraceRecordsWriteShapeWithoutPath(t *testing.T) {
	var recorded []string
	filesystem := &fuseFilesystem{recorder: func(operation string) {
		recorded = append(recorded, operation)
	}}
	privatePath := "/sessions/private-session.jsonl"
	filesystem.recordOpen("open", privatePath, 0x9, os.O_WRONLY|os.O_APPEND, 17, 0)
	filesystem.recordIO("write", privatePath, 17, 1234, 89, 89)
	filesystem.recordIO("read", privatePath, 17, 1200, 64, 64)
	filesystem.recordHandleResult("flush", privatePath, 17, 0)

	joined := strings.Join(recorded, "\n")
	for _, field := range []string{
		"open kind=session flags=0x9 translated=0x9 handle=17 result=0",
		"write kind=session handle=17 offset=1234 bytes=89 result=89",
		"read kind=session handle=17 offset=1200 bytes=64 result=64",
		"flush kind=session handle=17 result=0",
	} {
		if !strings.Contains(joined, field) {
			t.Fatalf("operation trace missing %q: %s", field, joined)
		}
	}
	if strings.Contains(joined, privatePath) || strings.Contains(joined, "private-session") {
		t.Fatalf("operation trace exposed a session path: %s", joined)
	}
}

func TestOpenExUsesDirectIOOnlyForWritableSessions(t *testing.T) {
	source := []byte("{\"record\":0}\n")
	managed := mountSessionFixture(t, "direct-io", source)
	core := New()
	if err := core.AddSession("direct-io", managed); err != nil {
		t.Fatal(err)
	}
	filesystem := &fuseFilesystem{core: core}

	readOnly := fuse.FileInfo_t{Flags: fuse.O_RDONLY}
	if result := filesystem.OpenEx("/direct-io.jsonl", &readOnly); result != 0 {
		t.Fatalf("read-only OpenEx result=%d", result)
	}
	if readOnly.DirectIo {
		t.Fatal("read-only session unexpectedly enabled direct I/O")
	}
	if errno := core.Release(readOnly.Fh); errno != 0 {
		t.Fatalf("release read-only handle errno=%v", errno)
	}

	writable := fuse.FileInfo_t{Flags: fuse.O_RDWR}
	if result := filesystem.OpenEx("/direct-io.jsonl", &writable); result != 0 {
		t.Fatalf("writable OpenEx result=%d", result)
	}
	if !writable.DirectIo {
		t.Fatal("writable session did not enable direct I/O")
	}
	if errno := core.Release(writable.Fh); errno != 0 {
		t.Fatalf("release writable handle errno=%v", errno)
	}
}

func TestFuseStatfsFallsBackToMountParentWithoutNativeRoot(t *testing.T) {
	filesystem := &fuseFilesystem{core: New(), statRoot: t.TempDir()}
	var stat fuse.Statfs_t
	if result := filesystem.Statfs("/", &stat); result != 0 {
		t.Fatalf("Statfs result=%d", result)
	}
	if stat.Bsize == 0 || stat.Blocks == 0 {
		t.Fatalf("Statfs returned no backing capacity: %#v", stat)
	}
}

func TestRealFuseMountNativeFileOperations(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
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
	identity, err := os.ReadFile(filepath.Join(mountPoint, ".codexfold-health"))
	if err != nil || len(identity) < 16 {
		t.Fatalf("mount identity file is unavailable: size=%d err=%v", len(identity), err)
	}
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
	hotSource := []byte("hot\n")
	hotSession := mountSessionFixture(t, "hot", hotSource)
	var hotAvailable atomic.Bool
	filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
		if sessionID != "hot" || !hotAvailable.Load() {
			return nil, os.ErrNotExist
		}
		return hotSession, nil
	})
	hotAvailable.Store(true)
	hotTarget := filepath.Join(mountPoint, "hot.jsonl")
	waitForRealFile(t, hotTarget, hotSource)
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
	if _, err := unix.FcntlInt(appendFile.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		_ = appendFile.Close()
		t.Fatalf("F_FULLFSYNC: %v", err)
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

func TestRealFuseTReadAndFsyncPerformance(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	line := []byte("{\"payload\":\"0123456789abcdef0123456789abcdef0123456789abcdef\"}\n")
	source := bytes.Repeat(line, (32<<20)/len(line)+1)
	source = source[:32<<20]
	nativeReadPath := filepath.Join(root, "native-read.jsonl")
	if err := os.WriteFile(nativeReadPath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	managed := mountSessionFixture(t, "darwin-performance", source)
	filesystem := New()
	if err := filesystem.AddSession("darwin-performance", managed); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealMount(t, mountPoint, filesystem)
	target := filepath.Join(mountPoint, "darwin-performance.jsonl")

	nativeRead := bestSequentialRead(t, nativeReadPath, int64(len(source)), 3)
	fuseRead := bestSequentialRead(t, target, int64(len(source)), 3)
	ratio := fuseRead / nativeRead
	if fuseRead < 1024 || ratio < 0.25 {
		t.Fatalf("FUSE-T read %.2f MiB/s is %.1f%% of APFS %.2f MiB/s", fuseRead, ratio*100, nativeRead)
	}

	nativeAppendPath := filepath.Join(root, "native-append.jsonl")
	if err := os.WriteFile(nativeAppendPath, []byte("{\"record\":0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nativeP95 := appendFsyncP95(t, nativeAppendPath, 100)
	fuseP95 := appendFsyncP95(t, target, 100)
	ceiling := 50 * time.Millisecond
	if relative := nativeP95 * 5; relative > ceiling {
		ceiling = relative
	}
	if fuseP95 > ceiling {
		t.Fatalf("FUSE-T append+fsync p95 %s exceeds ceiling %s; APFS p95=%s", fuseP95, ceiling, nativeP95)
	}
	t.Logf("FUSE-T read=%.2f MiB/s APFS=%.2f MiB/s ratio=%.1f%% append_fsync_p95=%s APFS_p95=%s", fuseRead, nativeRead, ratio*100, fuseP95, nativeP95)

	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func bestSequentialRead(t *testing.T, path string, size int64, rounds int) float64 {
	t.Helper()
	best := float64(0)
	for range rounds {
		started := time.Now()
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		readBytes, copyErr := io.Copy(io.Discard, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || readBytes != size {
			t.Fatalf("sequential read %q bytes=%d copy=%v close=%v", path, readBytes, copyErr, closeErr)
		}
		throughput := float64(readBytes) / (1024 * 1024) / time.Since(started).Seconds()
		if throughput > best {
			best = throughput
		}
	}
	return best
}

func appendFsyncP95(t *testing.T, path string, rounds int) time.Duration {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	durations := make([]time.Duration, 0, rounds)
	for index := range rounds {
		started := time.Now()
		if _, err := fmt.Fprintf(file, "{\"append\":%d}\n", index); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		durations = append(durations, time.Since(started))
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations[(len(durations)*95+99)/100-1]
}

func TestRealFuseManagedStaleTailOffsetsPreserveJSONL(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	source := []byte("{\"record\":0}\n")
	managed := mountSessionFixture(t, "stale-tail", source)
	filesystem := New()
	if err := filesystem.AddSession("stale-tail", managed); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	var traceMu sync.Mutex
	var trace []string
	options := HostOptions{
		MountPoint: mountPoint,
		Filesystem: filesystem,
		Foreground: true,
		OperationRecorder: func(operation string) {
			traceMu.Lock()
			defer traceMu.Unlock()
			trace = append(trace, operation)
		},
	}
	stopMount := startRealMountWithOptions(t, options)
	var mountStat unix.Statfs_t
	if err := unix.Statfs(mountPoint, &mountStat); err != nil {
		t.Fatal(err)
	}
	if mountStat.Flags&unix.MNT_SYNCHRONOUS == 0 {
		t.Fatal("real FUSE-T mount was reported healthy before synchronous I/O was enabled")
	}
	target := filepath.Join(mountPoint, "stale-tail.jsonl")
	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := []byte("{\"record\":1}\n")
	second := []byte("{\"record\":2}\n")
	staleEOF := int64(len(source))
	if _, err := file.WriteAt(first, staleEOF); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteAt(second, staleEOF); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), source...), first...), second...)
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, want) {
		traceMu.Lock()
		defer traceMu.Unlock()
		t.Fatalf("stale-tail visible bytes differ: got=%q want=%q err=%v trace=%q", got, want, err, trace)
	}
	if state := managed.State(); state.BackingPath != "" {
		t.Fatalf("stale-tail writes created backing %q", state.BackingPath)
	}
	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func TestRealFuseCanonicalNativeToManagedCutover(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := filepath.Join("archived_sessions", "rollout-cutover.jsonl")
	nativePath := filepath.Join(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte("{\"cutover\":true}\n")
	if err := os.WriteFile(nativePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealMount(t, mountPoint, filesystem)
	target := filepath.Join(mountPoint, route)

	managed := mountSessionFixture(t, "cutover", source)
	if err := filesystem.UpsertSessionAt("cutover", "/"+filepath.ToSlash(route), managed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}
	waitForRealFile(t, target, source)

	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func TestRealFuseCanonicalManagedRemovalRevealsNative(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := filepath.Join("sessions", "2026", "07", "14", "rollout-rollback.jsonl")
	nativePath := filepath.Join(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	managedBytes := []byte("{\"managed\":true}\n")
	nativeBytes := []byte("{\"native\":true}\n")
	managed := mountSessionFixture(t, "rollback", managedBytes)

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	if err := filesystem.AddSessionAt("rollback", "/"+filepath.ToSlash(route), managed); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealMount(t, mountPoint, filesystem)
	target := filepath.Join(mountPoint, route)
	waitForRealFile(t, target, managedBytes)

	if err := os.WriteFile(nativePath, nativeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.RemoveSession("rollback"); err != nil {
		t.Fatal(err)
	}
	waitForRealFileTransition(t, target, nativeBytes)

	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func TestRealFuseCanonicalManagedRemovalCanBeReaddedAtSamePath(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := filepath.Join("sessions", "2026", "07", "14", "rollout-republish.jsonl")
	nativePath := filepath.Join(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	firstManagedBytes := []byte("{\"generation\":1}\n")
	nativeBytes := []byte("{\"native\":true}\n")
	digest := sha256.Sum256(firstManagedBytes)
	digestHex := hex.EncodeToString(digest[:])
	managedRoot := filepath.Join(root, "managed")
	managedNativePath := filepath.Join(root, "managed-native.jsonl")
	if err := os.WriteFile(managedNativePath, firstManagedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := fold.Manifest{
		Version: fold.ManifestVersion, Kind: fold.ManifestKind,
		Session: fold.ManifestSession{ID: "republish", RolloutPath: managedNativePath},
		Source:  fold.ManifestSource{Bytes: int64(len(firstManagedBytes)), SHA256: digestHex},
		Parts:   []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: digestHex, RawBytes: int64(len(firstManagedBytes))}}},
	}
	managedOptions := vfs.SessionOptions{
		Root: managedRoot, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest,
		Reader: fuseFixtureReader{digestHex: firstManagedBytes},
		NativeSnapshot: vfs.NativeFile{
			Path: managedNativePath, Bytes: int64(len(firstManagedBytes)), SHA256: digestHex,
		},
	}
	firstManaged, err := vfs.OpenSession(context.Background(), managedOptions)
	if err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	if err := filesystem.AddSessionAt("republish", "/"+filepath.ToSlash(route), firstManaged); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalHome := filepath.Join(root, "home")
	if err := os.MkdirAll(canonicalHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(mountPoint, "sessions"), filepath.Join(canonicalHome, "sessions")); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealMount(t, mountPoint, filesystem)
	target := filepath.Join(canonicalHome, route)
	waitForRealFile(t, target, firstManagedBytes)

	if err := os.WriteFile(nativePath, nativeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Dir(firstManaged.State().DeltaPath)
	retiredStateDirectory := stateDirectory + ".retired"
	if err := os.Rename(stateDirectory, retiredStateDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(target); err == nil {
		t.Fatal("managed read unexpectedly succeeded after its state directory was retired")
	}
	if err := filesystem.RemoveSession("republish"); err != nil {
		t.Fatal(err)
	}
	waitForRealFileTransition(t, target, nativeBytes)
	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}
	waitForRealFileMissing(t, target)
	if err := os.Rename(retiredStateDirectory, stateDirectory); err != nil {
		t.Fatal(err)
	}
	republished, err := vfs.RepublishSessionState(filepath.Join(stateDirectory, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if republished.Generation != 2 {
		t.Fatalf("republished generation = %d, want 2", republished.Generation)
	}
	secondManaged, err := vfs.OpenSession(context.Background(), managedOptions)
	if err != nil {
		t.Fatal(err)
	}

	if err := filesystem.UpsertSessionAt("republish", "/"+filepath.ToSlash(route), secondManaged); err != nil {
		t.Fatal(err)
	}
	waitForRealFile(t, target, firstManagedBytes)

	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func TestRealFuseCanonicalNativePreferenceNeverLosesPath(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := filepath.Join("sessions", "2026", "07", "14", "rollout-native-preference.jsonl")
	nativePath := filepath.Join(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	managedBytes := []byte("{\"managed\":true}\n")
	nativeBytes := []byte("{\"native\":true}\n")
	if err := os.WriteFile(nativePath, nativeBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	if err := filesystem.AddSessionAt("native-preference", "/"+filepath.ToSlash(route), mountSessionFixture(t, "native-preference", managedBytes)); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealMount(t, mountPoint, filesystem)
	target := filepath.Join(mountPoint, route)
	waitForRealFile(t, target, managedBytes)

	if err := filesystem.PreferNativeSession("native-preference"); err != nil {
		t.Fatal(err)
	}
	waitForRealFileTransition(t, target, nativeBytes)
	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}
	waitForRealFileTransition(t, target, managedBytes)
	if err := os.WriteFile(nativePath, nativeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForRealFileTransition(t, target, nativeBytes)

	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func TestRealFuseCanonicalNativeAppendTransactionSurvivesRestart(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := filepath.Join("sessions", "2026", "07", "16", "rollout-native-transaction.jsonl")
	nativePath := filepath.Join(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":0}\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mountPoint, route)
	var traceMu sync.Mutex
	var trace []string
	start := func() func() {
		filesystem := NewCanonical()
		filesystem.SetNativeRoot(nativeRoot)
		if err := filesystem.RecoverNativeAppendTransactions(); err != nil {
			t.Fatal(err)
		}
		return startRealMountWithOptions(t, HostOptions{
			MountPoint: mountPoint, Filesystem: filesystem, Foreground: true,
			OperationRecorder: func(operation string) {
				traceMu.Lock()
				trace = append(trace, operation)
				traceMu.Unlock()
			},
		})
	}

	stopMount := start()
	large := append([]byte("{\"large\":\""), bytes.Repeat([]byte("x"), 1<<20)...)
	large = append(large, []byte("\"}\n")...)
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := file.Write(large); err != nil || n != len(large) {
		_ = file.Close()
		t.Fatalf("write large append: n=%d err=%v", n, err)
	}
	want := append(append([]byte(nil), base...), large...)
	backingAfterWrite, err := os.ReadFile(nativePath)
	if err != nil || (!bytes.Equal(backingAfterWrite, base) && !bytes.Equal(backingAfterWrite, want)) {
		_ = file.Close()
		traceMu.Lock()
		currentTrace := strings.Join(trace, "\n")
		traceMu.Unlock()
		t.Fatalf("backing exposed a partial transaction: bytes=%d err=%v trace=%s", len(backingAfterWrite), err, currentTrace)
	}
	visible, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(visible, want) {
		_ = file.Close()
		t.Fatalf("pending mounted view: bytes=%d err=%v want=%d", len(visible), err, len(want))
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatalf("fsync large append: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close large append: %v", err)
	}
	assertNativeBytes(t, nativePath, want)

	stopMount()
	waitForRealUnmount(t, mountPoint)
	stopMount = start()
	waitForRealFile(t, target, want)

	afterRestart := []byte("{\"after_restart\":true}\n")
	file, err = os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := file.Write(afterRestart); err != nil || n != len(afterRestart) {
		_ = file.Close()
		t.Fatalf("write after restart: n=%d err=%v", n, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("release commit after restart: %v", err)
	}
	want = append(want, afterRestart...)
	assertNativeBytes(t, nativePath, want)
	if !completeJSONL(want) {
		t.Fatal("real FUSE append produced invalid JSONL")
	}

	traceMu.Lock()
	joined := strings.Join(trace, "\n")
	traceMu.Unlock()
	for _, marker := range []string{"open kind=session", "write kind=session", "fsync", "flush", "release"} {
		if !strings.Contains(joined, marker) {
			t.Fatalf("real append trace missing %q: %s", marker, joined)
		}
	}
	if writes := strings.Count(joined, "write kind=session"); writes < 30 {
		t.Fatalf("large append was not exercised as split FUSE writes: writes=%d", writes)
	}
	for _, entry := range strings.Split(joined, "\n") {
		if strings.Contains(entry, "kind=session") && strings.Contains(entry, "result=-") {
			t.Fatalf("session operation failed in real append trace: %s", entry)
		}
	}
	for _, entry := range strings.Split(joined, "\n") {
		if strings.Contains(entry, "kind=appledouble") && strings.Contains(entry, "result=-5") {
			t.Fatalf("AppleDouble metadata was routed through JSONL validation: %s", entry)
		}
	}
	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func TestRealFuseMountCanonicalManagedRename(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE_TEST=1 to run the real FUSE-T adapter test")
	}
	root := t.TempDir()
	source := []byte("canonical-session\n")
	managed := mountSessionFixture(t, "fixture", source)
	nativeRoot := filepath.Join(root, "native")
	nativeActiveDirectory := filepath.Join(nativeRoot, "sessions", "2026", "07", "12")
	if err := os.MkdirAll(nativeActiveDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(nativeRoot, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	filename := "rollout-2026-07-12T14-28-28-fixture.jsonl"
	archivedPath := "/archived_sessions/" + filename
	if err := filesystem.AddSessionAt("fixture", archivedPath, managed); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	var recordedMu sync.Mutex
	var recorded []string
	stopMount := startRealMountWithOptions(t, HostOptions{
		MountPoint: mountPoint, Filesystem: filesystem, Foreground: true,
		OperationRecorder: func(operation string) {
			recordedMu.Lock()
			recorded = append(recorded, operation)
			recordedMu.Unlock()
		},
	})
	archivedTarget := filepath.Join(mountPoint, "archived_sessions", filename)
	activeTarget := filepath.Join(mountPoint, "sessions", "2026", "07", "12", filename)
	attributeName := "com.codexfold.test"
	attributeValue := []byte("persistent-metadata")
	if err := unix.Setxattr(archivedTarget, attributeName, attributeValue, 0); err != nil {
		t.Fatalf("set managed xattr: %v", err)
	}
	archivedSidecar := filepath.Join(nativeRoot, "archived_sessions", "._"+filename)
	activeSidecar := filepath.Join(nativeActiveDirectory, "._"+filename)
	sidecar, err := os.ReadFile(archivedSidecar)
	if err != nil || !bytes.Contains(sidecar, []byte(attributeName)) || !bytes.Contains(sidecar, attributeValue) {
		t.Fatalf("AppleDouble sidecar did not preserve xattr: bytes=%d err=%v", len(sidecar), err)
	}
	if err := os.Rename(archivedTarget, activeTarget); err != nil {
		t.Fatalf("rename canonical managed session: %v", err)
	}
	if _, err := os.Stat(archivedTarget); !os.IsNotExist(err) {
		t.Fatalf("archived path remained after rename: %v", err)
	}
	got, err := os.ReadFile(activeTarget)
	if err != nil || !bytes.Equal(got, source) {
		t.Fatalf("active managed bytes differ: got=%q err=%v", got, err)
	}
	if _, err := os.Stat(archivedSidecar); !os.IsNotExist(err) {
		t.Fatalf("archived AppleDouble sidecar remained after rename: %v", err)
	}
	movedSidecar, err := os.ReadFile(activeSidecar)
	if err != nil || !bytes.Contains(movedSidecar, []byte(attributeName)) || !bytes.Contains(movedSidecar, attributeValue) {
		t.Fatalf("active AppleDouble sidecar lost xattr: bytes=%d err=%v", len(movedSidecar), err)
	}
	if err := os.Rename(activeTarget, archivedTarget); err != nil {
		t.Fatalf("rename canonical managed session back: %v", err)
	}
	if _, err := os.Stat(activeTarget); !os.IsNotExist(err) {
		t.Fatalf("active path remained after reverse rename: %v", err)
	}
	if _, err := os.Stat(activeSidecar); !os.IsNotExist(err) {
		t.Fatalf("active AppleDouble sidecar remained after reverse rename: %v", err)
	}
	restoredSidecar, err := os.ReadFile(archivedSidecar)
	if err != nil || !bytes.Contains(restoredSidecar, []byte(attributeName)) || !bytes.Contains(restoredSidecar, attributeValue) {
		t.Fatalf("restored AppleDouble sidecar lost xattr: bytes=%d err=%v", len(restoredSidecar), err)
	}
	recordedMu.Lock()
	joined := strings.Join(recorded, ",")
	recordedMu.Unlock()
	for _, operation := range []string{"getattr", "rename", "open", "read", "release"} {
		if !strings.Contains(joined, operation) {
			t.Fatalf("operation trace missing %q: %s", operation, joined)
		}
	}
	stopMount()
	waitForRealUnmount(t, mountPoint)
}

func startRealMount(t *testing.T, mountPoint string, filesystem *Filesystem) func() {
	return startRealMountWithOptions(t, HostOptions{MountPoint: mountPoint, Filesystem: filesystem, Foreground: true})
}

func startRealMountWithOptions(t *testing.T, options HostOptions) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	mountDone := make(chan error, 1)
	go func() {
		mountDone <- Mount(ctx, options)
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
	waitForRealMount(t, options.MountPoint, mountDone)
	return stopMount
}

func waitForRealMount(t *testing.T, mountPoint string, mountDone <-chan error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastProbeErr error
	for time.Now().Before(deadline) {
		if err := service.ProbeMount(mountPoint); err == nil {
			return
		} else {
			lastProbeErr = err
		}
		select {
		case err := <-mountDone:
			t.Fatalf("mount exited before becoming healthy: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("FUSE mount did not become healthy: %v", lastProbeErr)
}

func waitForRealUnmount(t *testing.T, mountPoint string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var stat unix.Statfs_t
		if err := unix.Statfs(mountPoint, &stat); err != nil || !sameRealMountPath(unix.ByteSliceToString(stat.Mntonname[:]), mountPoint) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("FUSE mount remained active after shutdown")
}

func sameRealMountPath(left string, right string) bool {
	canonical := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Clean(resolved)
		}
		return filepath.Clean(path)
	}
	return canonical(left) == canonical(right)
}

func waitForRealFile(t *testing.T, path string, want []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if !bytes.Equal(data, want) {
				t.Fatalf("hot-loaded file differs: got=%q want=%q", data, want)
			}
			return
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read hot-loaded file: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("hot-loaded file did not become visible")
}

func waitForRealFileTransition(t *testing.T, path string, want []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && bytes.Equal(data, want) {
			return
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read transitioned file: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("managed file did not transition to native bytes")
}

func waitForRealFileMissing(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		} else if err != nil {
			t.Fatalf("stat transitioned file: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("canonical file did not become absent")
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
