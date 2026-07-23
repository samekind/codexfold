//go:build darwin

package mountfs

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fskitproto"
	"golang.org/x/sys/unix"
)

const (
	nativeFSKitMountEnv                = "CODEXFOLD_NATIVE_FSKIT_MOUNT"
	nativeFSKitNativeRootEnv           = "CODEXFOLD_NATIVE_FSKIT_NATIVE_ROOT"
	nativeFSKitResourceEnv             = "CODEXFOLD_NATIVE_FSKIT_RESOURCE"
	nativeFSKitVirtualFileEnv          = "CODEXFOLD_NATIVE_FSKIT_VIRTUAL_FILE"
	nativeFSKitVirtualReferenceFileEnv = "CODEXFOLD_NATIVE_FSKIT_VIRTUAL_REFERENCE_FILE"
)

func TestNativeFSKitMountedMetadataAndXattrs(t *testing.T) {
	root := nativeFSKitMountedTestRoot(t)
	target := filepath.Join(root, "metadata.bin")
	writeMountedTestFile(t, target, []byte("{}\n"))

	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Chown(target, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chown: %v", err)
	}
	atime := time.Unix(1_720_000_000, 123_000_000)
	mtime := time.Unix(1_720_000_100, 456_000_000)
	if err := os.Chtimes(target, atime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat payload = %T", info.Sys())
	}
	if info.Mode().Perm() != 0o640 || stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
		t.Fatalf("metadata = mode %#o uid %d gid %d", info.Mode().Perm(), stat.Uid, stat.Gid)
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("mtime = %s, want %s", info.ModTime(), mtime)
	}
	gotAtime := time.Unix(stat.Atimespec.Sec, stat.Atimespec.Nsec)
	if !gotAtime.Equal(atime) {
		t.Fatalf("atime = %s, want %s", gotAtime, atime)
	}

	attribute := "vip.jstar.codexfold.integration"
	first := []byte("first")
	if err := unix.Setxattr(target, attribute, first, unix.XATTR_CREATE); err != nil {
		t.Fatalf("create xattr: %v", err)
	}
	if err := unix.Setxattr(target, attribute, []byte("duplicate"), unix.XATTR_CREATE); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("duplicate create xattr error = %v, want EEXIST", err)
	}
	second := []byte("second")
	if err := unix.Setxattr(target, attribute, second, unix.XATTR_REPLACE); err != nil {
		t.Fatalf("replace xattr: %v", err)
	}
	if got := mountedTestXattr(t, target, attribute); !bytes.Equal(got, second) {
		t.Fatalf("xattr value = %q, want %q", got, second)
	}
	if names := mountedTestXattrNames(t, target); !slices.Contains(names, attribute) {
		t.Fatalf("xattr names = %v, missing %s", names, attribute)
	}
	finderInfo := make([]byte, 32)
	copy(finderInfo, []byte("CodexFold-FSKit"))
	if err := unix.Setxattr(target, "com.apple.FinderInfo", finderInfo, 0); err != nil {
		t.Fatalf("set FinderInfo xattr: %v", err)
	}
	if got := mountedTestXattr(t, target, "com.apple.FinderInfo"); !bytes.Equal(got, finderInfo) {
		t.Fatalf("FinderInfo xattr = %x, want %x", got, finderInfo)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), "._"+filepath.Base(target))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected AppleDouble sidecar: %v", err)
	}
	if err := unix.Removexattr(target, attribute); err != nil {
		t.Fatalf("remove xattr: %v", err)
	}
	if _, err := readMountedTestXattr(target, attribute); !errors.Is(err, syscall.ENOATTR) {
		t.Fatalf("removed xattr error = %v, want ENOATTR", err)
	}
	if err := unix.Setxattr(target, attribute, []byte("missing"), unix.XATTR_REPLACE); !errors.Is(err, syscall.ENOATTR) {
		t.Fatalf("replace missing xattr error = %v, want ENOATTR", err)
	}
}

func TestNativeFSKitMountedWritesAndFullSync(t *testing.T) {
	root := nativeFSKitMountedTestRoot(t)
	target := filepath.Join(root, "writes.bin")
	writeMountedTestFile(t, target, []byte("0123456789\n"))

	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("AB"), 2); err != nil {
		file.Close()
		t.Fatalf("random overwrite: %v", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatalf("fsync: %v", err)
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		file.Close()
		t.Fatalf("F_FULLFSYNC: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	assertMountedTestContent(t, target, []byte("01AB456789\n"))

	if err := os.Truncate(target, 6); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	appendFile, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendFile.Write([]byte("TAIL")); err != nil {
		appendFile.Close()
		t.Fatalf("append: %v", err)
	}
	if err := appendFile.Sync(); err != nil {
		appendFile.Close()
		t.Fatalf("append fsync: %v", err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}
	assertMountedTestContent(t, target, []byte("01AB45TAIL"))

	oldEOF := filepath.Join(root, "old-eof.jsonl")
	base := []byte("{\"record\":0}\n")
	first := []byte("{\"record\":1}\n")
	second := []byte("{\"record\":2}\n")
	third := []byte("{\"record\":3}\n")
	writeMountedTestFile(t, oldEOF, base)
	oldEOFFile, err := os.OpenFile(oldEOF, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldEOFFile.WriteAt(first, int64(len(base))); err != nil {
		oldEOFFile.Close()
		t.Fatalf("first old-EOF write: %v", err)
	}
	if _, err := oldEOFFile.WriteAt(second, int64(len(base))); err != nil {
		oldEOFFile.Close()
		t.Fatalf("second old-EOF write: %v", err)
	}
	if _, err := oldEOFFile.WriteAt(third, int64(len(base))); err != nil {
		oldEOFFile.Close()
		t.Fatalf("third old-EOF write after cache revoke: %v", err)
	}
	if err := oldEOFFile.Close(); err != nil {
		t.Fatal(err)
	}
	want := append(append(append(append([]byte(nil), base...), first...), second...), third...)
	assertNativeFSKitBackingContent(t, oldEOF, want)
	assertMountedTestContent(t, oldEOF, want)
}

func TestNativeFSKitMountedOverlappingOpenLifetime(t *testing.T) {
	root := nativeFSKitMountedTestRoot(t)
	target := filepath.Join(root, "overlapping-open.jsonl")
	writeMountedTestFile(t, target, []byte("{\"record\":0}\n"))

	first, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		second.Close()
		t.Fatal(err)
	}
	appended := []byte("{\"record\":1}\n")
	if _, err := second.Write(appended); err != nil {
		second.Close()
		t.Fatalf("write through surviving open descriptor: %v", err)
	}
	if err := second.Sync(); err != nil {
		second.Close()
		t.Fatalf("sync through surviving open descriptor: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	assertMountedTestContent(t, target, append([]byte("{\"record\":0}\n"), appended...))
}

func TestNativeFSKitMountedNamespaceOperations(t *testing.T) {
	root := nativeFSKitMountedTestRoot(t)
	source := filepath.Join(root, "source.jsonl")
	destination := filepath.Join(root, "destination.jsonl")
	writeMountedTestFile(t, source, []byte("source\n"))
	writeMountedTestFile(t, destination, []byte("destination\n"))
	if err := os.Rename(source, destination); err != nil {
		t.Fatalf("overwrite rename: %v", err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed source still exists: %v", err)
	}
	assertMountedTestContent(t, destination, []byte("source\n"))

	nested := filepath.Join(root, "nested", "child")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Remove(nested); err != nil {
		t.Fatalf("rmdir child: %v", err)
	}
	if err := os.Remove(filepath.Dir(nested)); err != nil {
		t.Fatalf("rmdir parent: %v", err)
	}

	if err := os.Symlink(destination, filepath.Join(root, "symbolic")); !isNotSupported(err) {
		t.Fatalf("symlink error = %v, want ENOTSUP", err)
	}
	if err := os.Link(destination, filepath.Join(root, "hard")); !isNotSupported(err) {
		t.Fatalf("hardlink error = %v, want ENOTSUP", err)
	}
}

func TestNativeFSKitMountedSamePathRecreate(t *testing.T) {
	root := nativeFSKitMountedTestRoot(t)
	target := filepath.Join(root, "same-path.jsonl")
	for round := 1; round <= 20; round++ {
		first := []byte(fmt.Sprintf("{\"round\":%d,\"phase\":\"first\"}\n", round))
		second := []byte(fmt.Sprintf("{\"round\":%d,\"phase\":\"second\"}\n", round))
		writeMountedTestFile(t, target, first)
		assertMountedTestContent(t, target, first)
		if err := os.Remove(target); err != nil {
			t.Fatalf("round %d remove first generation: %v", round, err)
		}
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("round %d removed path stat = %v, want ENOENT", round, err)
		}
		writeMountedTestFile(t, target, second)
		assertMountedTestContent(t, target, second)
		if err := os.Remove(target); err != nil {
			t.Fatalf("round %d remove second generation: %v", round, err)
		}
	}
}

func TestNativeFSKitMountedArchiveRoundTripAndMmap(t *testing.T) {
	mountPoint := nativeFSKitMountPoint(t)
	relativeDirectory := filepath.Join("2099", "12", "31")
	activeDirectory := filepath.Join(mountPoint, "sessions", relativeDirectory)
	archiveDirectory := filepath.Join(mountPoint, "archived_sessions", relativeDirectory)
	if err := os.MkdirAll(activeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archiveDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("archive-roundtrip-%d.jsonl", time.Now().UnixNano())
	active := filepath.Join(activeDirectory, name)
	archived := filepath.Join(archiveDirectory, name)
	t.Cleanup(func() {
		_ = os.Remove(active)
		_ = os.Remove(archived)
	})
	content := []byte("{\"archive\":true}\n")
	writeMountedTestFile(t, active, content)

	file, err := os.Open(active)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := unix.Mmap(int(file.Fd()), 0, len(content), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		file.Close()
		t.Fatalf("mmap: %v", err)
	}
	if !bytes.Equal(mapped, content) {
		unix.Munmap(mapped)
		file.Close()
		t.Fatalf("mmap bytes = %q, want %q", mapped, content)
	}
	if err := unix.Munmap(mapped); err != nil {
		file.Close()
		t.Fatalf("munmap: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(active, archived); err != nil {
		t.Fatalf("archive rename: %v", err)
	}
	assertMountedTestContent(t, archived, content)
	if err := os.Rename(archived, active); err != nil {
		t.Fatalf("unarchive rename: %v", err)
	}
	assertMountedTestContent(t, active, content)
}

func TestNativeFSKitMountedOpenUnlink(t *testing.T) {
	root := nativeFSKitMountedTestRoot(t)
	target := filepath.Join(root, "open-unlink.jsonl")
	writeMountedTestFile(t, target, []byte("before"))

	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		file.Close()
		t.Fatalf("unlink open file: %v", err)
	}
	if _, err := file.WriteAt([]byte("after"), 0); err != nil {
		file.Close()
		t.Fatalf("write unlinked file: %v", err)
	}
	buffer := make([]byte, 6)
	if _, err := file.ReadAt(buffer, 0); err != nil {
		file.Close()
		t.Fatalf("read unlinked file: %v", err)
	}
	if !bytes.Equal(buffer, []byte("aftere")) {
		file.Close()
		t.Fatalf("unlinked content = %q", buffer)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unlinked path exists after close: %v", err)
	}
}

func TestNativeFSKitMountedExternalNamespaceRefresh(t *testing.T) {
	mountPoint := nativeFSKitMountPoint(t)
	nativeRoot := os.Getenv(nativeFSKitNativeRootEnv)
	if nativeRoot == "" {
		t.Skipf("set %s to run external namespace refresh", nativeFSKitNativeRootEnv)
	}
	if !filepath.IsAbs(nativeRoot) {
		t.Fatalf("%s must be absolute", nativeFSKitNativeRootEnv)
	}

	relative := filepath.Join("sessions", "2099", "12", "31", fmt.Sprintf("external-%d.bin", time.Now().UnixNano()))
	nativePath := filepath.Join(nativeRoot, relative)
	mountedPath := filepath.Join(mountPoint, relative)
	parentRoute := "/" + filepath.ToSlash(filepath.Dir(relative))
	version, observeVersion := nativeFSKitMountedNamespaceVersion(t)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(nativePath) })
	// Prime the mounted kernel's negative name cache before the native-side
	// creator appears. This is the path a real Codex restart or file watcher
	// hits after observing a session path before its rollout exists.
	if _, err := os.Stat(mountedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncreated mounted path stat = %v, want not exist", err)
	}
	if err := os.WriteFile(nativePath, []byte("external-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if observeVersion {
		version = waitForNativeFSKitNamespaceAdvance(t, version, 3*time.Second)
		logNativeFSKitMountedEntry(t, "after create", parentRoute)
	}
	waitForMountedContent(t, mountedPath, []byte("external-one\n"), 3*time.Second)

	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}
	if observeVersion {
		version = waitForNativeFSKitNamespaceAdvance(t, version, 3*time.Second)
		logNativeFSKitMountedEntry(t, "after remove", parentRoute)
	}
	waitForMountedAbsence(t, mountedPath, 3*time.Second)
	if err := os.WriteFile(nativePath, []byte("external-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if observeVersion {
		version = waitForNativeFSKitNamespaceAdvance(t, version, 3*time.Second)
		logNativeFSKitMountedEntry(t, "after recreate", parentRoute)
		t.Logf("native namespace reached version %d after external recreation", version)
	}
	waitForMountedContent(t, mountedPath, []byte("external-two\n"), 3*time.Second)
}

func TestNativeFSKitMountedExternalDirectoryNamespaceRefresh(t *testing.T) {
	mountPoint := nativeFSKitMountPoint(t)
	nativeRoot := os.Getenv(nativeFSKitNativeRootEnv)
	if nativeRoot == "" {
		t.Skipf("set %s to run external directory refresh", nativeFSKitNativeRootEnv)
	}
	relative := filepath.Join("sessions", "2099", "12", "31", fmt.Sprintf("external-directory-%d", time.Now().UnixNano()))
	nativeDirectory := filepath.Join(nativeRoot, relative)
	mountedDirectory := filepath.Join(mountPoint, relative)
	mountedChild := filepath.Join(mountedDirectory, "child.jsonl")
	if _, err := os.Stat(mountedDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncreated mounted directory stat = %v, want not exist", err)
	}
	if _, err := os.Stat(mountedChild); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncreated mounted child stat = %v, want not exist", err)
	}

	version, observeVersion := nativeFSKitMountedNamespaceVersion(t)
	if err := os.MkdirAll(nativeDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	nativeChild := filepath.Join(nativeDirectory, "child.jsonl")
	want := []byte("external-directory-content\n")
	if err := os.WriteFile(nativeChild, want, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(nativeChild)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(nativeDirectory) })
	if observeVersion {
		version = waitForNativeFSKitNamespaceAdvance(t, version, 3*time.Second)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if info, statErr := os.Stat(mountedDirectory); statErr == nil && info.IsDir() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if info, err := os.Stat(mountedDirectory); err != nil || !info.IsDir() {
		t.Fatalf("mounted external directory = %v, want a directory", err)
	}
	waitForMountedContent(t, mountedChild, want, 3*time.Second)
	mountedInfo, err := os.Stat(mountedChild)
	if err != nil {
		t.Fatal(err)
	}
	if mountedInfo.Mode().Perm() != before.Mode().Perm() || mountedInfo.Size() != before.Size() {
		t.Fatalf("mounted child metadata mode=%#o size=%d, want mode=%#o size=%d", mountedInfo.Mode().Perm(), mountedInfo.Size(), before.Mode().Perm(), before.Size())
	}

	if err := os.RemoveAll(nativeDirectory); err != nil {
		t.Fatal(err)
	}
	if observeVersion {
		version = waitForNativeFSKitNamespaceAdvance(t, version, 3*time.Second)
	}
	waitForMountedAbsence(t, mountedChild, 3*time.Second)
	waitForMountedAbsence(t, mountedDirectory, 3*time.Second)
}

func TestNativeFSKitMountedPerformance(t *testing.T) {
	mountPoint := nativeFSKitMountPoint(t)
	nativeRoot := os.Getenv(nativeFSKitNativeRootEnv)
	if nativeRoot == "" {
		t.Skipf("set %s to run native FSKit performance", nativeFSKitNativeRootEnv)
	}
	root := nativeFSKitMountedTestRoot(t)
	relativeRoot, err := filepath.Rel(mountPoint, root)
	if err != nil {
		t.Fatal(err)
	}
	mountedPath := filepath.Join(root, "performance.bin")
	nativePath := filepath.Join(nativeRoot, relativeRoot, "performance.bin")
	const sourceBytes = int64(256 << 20)
	versionBefore, observeVersion := nativeFSKitMountedNamespaceVersion(t)
	if err := writePerformanceFixture(nativePath, sourceBytes); err != nil {
		t.Fatal(err)
	}
	nativeInfo, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	versionAfterWrite, _ := nativeFSKitMountedNamespaceVersion(t)
	mountedInfo, mountedErr := os.Stat(mountedPath)
	t.Logf(
		"native-fskit performance fixture native_size=%d mounted_size=%d mounted_err=%v namespace_before=%d namespace_after_write=%d observe_version=%t",
		nativeInfo.Size(), fileInfoSize(mountedInfo), mountedErr, versionBefore, versionAfterWrite, observeVersion,
	)
	logNativeFSKitMountedEntry(t, "performance-after-write", mountedPath)
	time.Sleep(time.Second)
	versionAfterSettle, _ := nativeFSKitMountedNamespaceVersion(t)
	mountedInfo, mountedErr = os.Stat(mountedPath)
	t.Logf(
		"native-fskit performance settle mounted_size=%d mounted_err=%v namespace_after_settle=%d",
		fileInfoSize(mountedInfo), mountedErr, versionAfterSettle,
	)
	logNativeFSKitMountedEntry(t, "performance-after-settle", mountedPath)
	waitForMountedSize(t, mountedPath, sourceBytes, 5*time.Second)

	nativeCold, nativeBypass, err := sequentialReadMetric(nativePath, true)
	if err != nil {
		t.Fatal(err)
	}
	mountedCold, mountedBypass, err := sequentialReadMetric(mountedPath, true)
	if err != nil {
		t.Fatal(err)
	}
	nativeWarm, err := medianSequentialThroughput(nativePath, 3)
	if err != nil {
		t.Fatal(err)
	}
	mountedWarm, err := medianSequentialThroughput(mountedPath, 3)
	if err != nil {
		t.Fatal(err)
	}
	nativeHash, err := streamingSHA256(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	mountedHash, err := streamingSHA256(mountedPath)
	if err != nil {
		t.Fatal(err)
	}
	if nativeHash != mountedHash {
		t.Fatal("mounted performance file differs from the native source")
	}
	t.Logf(
		"native-fskit performance pre-gate bytes=%d cold_native=%.2fMiB/s cold_mounted=%.2fMiB/s cold_ratio=%.3f warm_native=%.2fMiB/s warm_mounted=%.2fMiB/s warm_ratio=%.3f native_nocache=%t mounted_nocache=%t",
		sourceBytes,
		nativeCold/(1<<20), mountedCold/(1<<20), mountedCold/nativeCold,
		nativeWarm/(1<<20), mountedWarm/(1<<20), mountedWarm/nativeWarm,
		nativeBypass, mountedBypass,
	)
	const minimumThroughput = float64(500 << 20)
	if mountedCold < minimumThroughput || mountedWarm < minimumThroughput {
		t.Fatalf("mounted throughput below 500 MiB/s: cold=%.2f MiB/s warm=%.2f MiB/s", mountedCold/(1<<20), mountedWarm/(1<<20))
	}
	if mountedCold/nativeCold < 0.10 || mountedWarm/nativeWarm < 0.05 {
		t.Fatalf("mounted/native throughput ratio too low: cold=%.3f warm=%.3f", mountedCold/nativeCold, mountedWarm/nativeWarm)
	}

	latencyPath := filepath.Join(root, "append-fsync.jsonl")
	writeMountedTestFile(t, latencyPath, []byte("{}\n"))
	file, err := os.OpenFile(latencyPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	latencies := make([]time.Duration, 0, 100)
	for index := 0; index < 100; index++ {
		started := time.Now()
		if _, err := fmt.Fprintf(file, "{\"append_fsync\":%d}\n", index); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			t.Fatal(err)
		}
		latencies = append(latencies, time.Since(started))
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	p50 := durationPercentile(latencies, 0.50)
	p95 := durationPercentile(latencies, 0.95)
	p99 := durationPercentile(latencies, 0.99)
	if p95 > 100*time.Millisecond {
		t.Fatalf("append plus fsync p95 exceeded 100 ms: %s", p95)
	}
	t.Logf(
		"native-fskit performance bytes=%d cold_native=%.2fMiB/s cold_mounted=%.2fMiB/s cold_ratio=%.3f warm_native=%.2fMiB/s warm_mounted=%.2fMiB/s warm_ratio=%.3f native_nocache=%t mounted_nocache=%t append_fsync_p50=%s append_fsync_p95=%s append_fsync_p99=%s",
		sourceBytes,
		nativeCold/(1<<20), mountedCold/(1<<20), mountedCold/nativeCold,
		nativeWarm/(1<<20), mountedWarm/(1<<20), mountedWarm/nativeWarm,
		nativeBypass, mountedBypass, p50, p95, p99,
	)
}

func TestNativeFSKitMountedReadAheadCoherency(t *testing.T) {
	mountPoint := nativeFSKitMountPoint(t)
	nativeRoot := os.Getenv(nativeFSKitNativeRootEnv)
	if nativeRoot == "" {
		t.Skipf("set %s to run native FSKit read-ahead coherency", nativeFSKitNativeRootEnv)
	}
	root := nativeFSKitMountedTestRoot(t)
	relativeRoot, err := filepath.Rel(mountPoint, root)
	if err != nil {
		t.Fatal(err)
	}
	mountedPath := filepath.Join(root, "read-ahead.jsonl")
	nativePath := filepath.Join(nativeRoot, relativeRoot, "read-ahead.jsonl")
	const blockSize = 1 << 20
	const lineSize = 4096
	prefix := []byte("{\"payload\":\"")
	suffix := []byte("\"}\n")
	line := make([]byte, lineSize)
	copy(line, prefix)
	for index := len(prefix); index < len(line)-len(suffix); index++ {
		line[index] = byte('a' + index%26)
	}
	copy(line[len(line)-len(suffix):], suffix)
	content := bytes.Repeat(line, 4*blockSize/lineSize)
	if !completeJSONL(content) {
		t.Fatal("read-ahead fixture is not valid JSONL")
	}
	writeMountedTestFile(t, mountedPath, content)

	reader, err := os.Open(mountedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, testCase := range []struct {
		offset int64
		length int
	}{
		{offset: 0, length: 8192},
		{offset: blockSize - 4096, length: 8192},
		{offset: blockSize + 2048, length: 16384},
		{offset: 2*blockSize - 7777, length: 12000},
		{offset: 333333, length: 7777},
	} {
		assertOpenFileRange(t, reader, content, testCase.offset, testCase.length)
	}

	const concurrentReaders = 8
	const concurrentRounds = 64
	const concurrentReadBytes = 32 << 10
	concurrentErrors := make(chan error, concurrentReaders)
	var concurrent sync.WaitGroup
	for worker := 0; worker < concurrentReaders; worker++ {
		concurrent.Add(1)
		go func(worker int) {
			defer concurrent.Done()
			for round := 0; round < concurrentRounds; round++ {
				offset := int64((worker*7919 + round*65537) % (len(content) - concurrentReadBytes))
				buffer := make([]byte, concurrentReadBytes)
				n, readErr := reader.ReadAt(buffer, offset)
				if readErr != nil || n != len(buffer) || !bytes.Equal(buffer, content[offset:offset+int64(len(buffer))]) {
					concurrentErrors <- fmt.Errorf("worker=%d round=%d offset=%d bytes=%d error=%v", worker, round, offset, n, readErr)
					return
				}
			}
		}(worker)
	}
	concurrent.Wait()
	close(concurrentErrors)
	for concurrentErr := range concurrentErrors {
		t.Fatal(concurrentErr)
	}

	eofOffset := int64(len(content) - 100)
	eofBuffer := make([]byte, 4096)
	n, err := reader.ReadAt(eofBuffer, eofOffset)
	if !errors.Is(err, io.EOF) || n != 100 || !bytes.Equal(eofBuffer[:n], content[eofOffset:]) {
		t.Fatalf("EOF read n=%d err=%v", n, err)
	}

	writeOffset := int64(128<<10 + len(prefix) + 64)
	replacement := bytes.Repeat([]byte{'Z'}, 512)
	writer, err := os.OpenFile(mountedPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt(replacement, writeOffset); err != nil {
		writer.Close()
		t.Fatalf("mounted overwrite: %v", err)
	}
	if err := writer.Sync(); err != nil {
		writer.Close()
		t.Fatalf("mounted overwrite sync: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	copy(content[writeOffset:], replacement)
	if !completeJSONL(content) {
		t.Fatal("mounted overwrite made the fixture invalid JSONL")
	}
	assertOpenFileRange(t, reader, content, writeOffset, len(replacement))

	truncateSize := int64(blockSize + lineSize)
	if err := os.Truncate(mountedPath, truncateSize); err != nil {
		t.Fatalf("mounted truncate: %v", err)
	}
	truncated := content[:truncateSize]
	if !completeJSONL(truncated) {
		t.Fatal("truncated fixture is not valid JSONL")
	}
	truncateBuffer := make([]byte, 4096)
	n, err = reader.ReadAt(truncateBuffer, truncateSize-100)
	if !errors.Is(err, io.EOF) || n != 100 || !bytes.Equal(truncateBuffer[:n], truncated[truncateSize-100:]) {
		t.Fatalf("post-truncate read n=%d err=%v", n, err)
	}

	externalOffset := int64(64<<10 + len(prefix) + 32)
	externalReplacement := bytes.Repeat([]byte{'X'}, 256)
	native, err := os.OpenFile(nativePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.WriteAt(externalReplacement, externalOffset); err != nil {
		native.Close()
		t.Fatalf("native overwrite: %v", err)
	}
	if err := native.Sync(); err != nil {
		native.Close()
		t.Fatalf("native overwrite sync: %v", err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	copy(truncated[externalOffset:], externalReplacement)
	if !completeJSONL(truncated) {
		t.Fatal("external overwrite made the fixture invalid JSONL")
	}
	waitForOpenFileRange(t, reader, truncated, externalOffset, len(externalReplacement), 3*time.Second)

	renamedPath := filepath.Join(root, "read-ahead-renamed.jsonl")
	if err := os.Rename(mountedPath, renamedPath); err != nil {
		t.Fatalf("rename cached file: %v", err)
	}
	assertOpenFileRange(t, reader, truncated, 0, 8192)
	assertMountedTestContent(t, renamedPath, truncated)
}

func TestNativeFSKitMountedVirtualConcurrentReadAhead(t *testing.T) {
	virtualPath := os.Getenv(nativeFSKitVirtualFileEnv)
	referencePath := os.Getenv(nativeFSKitVirtualReferenceFileEnv)
	if virtualPath == "" || referencePath == "" {
		t.Skipf("set %s and %s to run packed virtual concurrent reads", nativeFSKitVirtualFileEnv, nativeFSKitVirtualReferenceFileEnv)
	}
	if !filepath.IsAbs(virtualPath) || !filepath.IsAbs(referencePath) {
		t.Fatal("packed virtual and reference paths must be absolute")
	}
	virtual, err := os.Open(virtualPath)
	if err != nil {
		t.Fatal(err)
	}
	defer virtual.Close()
	reference, err := os.Open(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	referenceInfo, err := reference.Stat()
	if err != nil {
		t.Fatal(err)
	}
	virtualInfo, err := virtual.Stat()
	if err != nil {
		t.Fatal(err)
	}
	const readers = 8
	const rounds = 8
	const readBytes = 32 << 10
	if referenceInfo.Size() < readBytes || virtualInfo.Size() < referenceInfo.Size() {
		t.Fatalf("virtual bytes=%d reference bytes=%d", virtualInfo.Size(), referenceInfo.Size())
	}

	errorsByReader := make(chan error, readers)
	var concurrent sync.WaitGroup
	for worker := 0; worker < readers; worker++ {
		concurrent.Add(1)
		go func(worker int) {
			defer concurrent.Done()
			for round := 0; round < rounds; round++ {
				limit := referenceInfo.Size() - readBytes
				offset := int64(worker*104729+round*15485863) % (limit + 1)
				got := make([]byte, readBytes)
				want := make([]byte, readBytes)
				gotN, gotErr := virtual.ReadAt(got, offset)
				wantN, wantErr := reference.ReadAt(want, offset)
				if gotErr != nil || wantErr != nil || gotN != readBytes || wantN != readBytes || !bytes.Equal(got, want) {
					errorsByReader <- fmt.Errorf("worker=%d round=%d offset=%d virtual=%d/%v reference=%d/%v", worker, round, offset, gotN, gotErr, wantN, wantErr)
					return
				}
			}
		}(worker)
	}
	concurrent.Wait()
	close(errorsByReader)
	for readErr := range errorsByReader {
		t.Fatal(readErr)
	}
}

func TestNativeFSKitMountedManagedPerformance(t *testing.T) {
	virtualPath := os.Getenv(nativeFSKitVirtualFileEnv)
	referencePath := os.Getenv(nativeFSKitVirtualReferenceFileEnv)
	if virtualPath == "" || referencePath == "" {
		t.Skipf("set %s and %s to run packed virtual performance", nativeFSKitVirtualFileEnv, nativeFSKitVirtualReferenceFileEnv)
	}
	if !filepath.IsAbs(virtualPath) || !filepath.IsAbs(referencePath) {
		t.Fatal("packed virtual and reference paths must be absolute")
	}

	referenceInfo, err := os.Stat(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	virtualInfo, err := os.Stat(virtualPath)
	if err != nil {
		t.Fatal(err)
	}
	if virtualInfo.Size() != referenceInfo.Size() {
		t.Fatalf("virtual bytes=%d reference bytes=%d", virtualInfo.Size(), referenceInfo.Size())
	}
	coldReferencePath, copyBypass, err := uncachedReferenceCopy(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(coldReferencePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove uncached reference copy: %v", err)
		}
	})
	if !copyBypass {
		t.Fatal("F_NOCACHE was not applied while creating the cold reference copy")
	}

	nativeCold, nativeBypass, err := sequentialReadMetric(coldReferencePath, true)
	if err != nil {
		t.Fatal(err)
	}
	virtualCold, virtualBypass, err := sequentialReadMetric(virtualPath, true)
	if err != nil {
		t.Fatal(err)
	}
	nativeWarm, err := medianSequentialThroughput(coldReferencePath, 3)
	if err != nil {
		t.Fatal(err)
	}
	virtualWarm, err := medianSequentialThroughput(virtualPath, 3)
	if err != nil {
		t.Fatal(err)
	}
	nativeHash, err := streamingSHA256(coldReferencePath)
	if err != nil {
		t.Fatal(err)
	}
	virtualHash, err := streamingSHA256(virtualPath)
	if err != nil {
		t.Fatal(err)
	}
	if nativeHash != virtualHash {
		t.Fatal("mounted managed file differs from its retained native snapshot")
	}

	coldRatio := virtualCold / nativeCold
	warmRatio := virtualWarm / nativeWarm
	t.Logf(
		"native-fskit managed performance bytes=%d cold_native=%.2fMiB/s cold_virtual=%.2fMiB/s cold_ratio=%.3f warm_native=%.2fMiB/s warm_virtual=%.2fMiB/s warm_ratio=%.3f native_nocache=%t virtual_nocache=%t",
		referenceInfo.Size(),
		nativeCold/(1<<20), virtualCold/(1<<20), coldRatio,
		nativeWarm/(1<<20), virtualWarm/(1<<20), warmRatio,
		nativeBypass, virtualBypass,
	)
	if !nativeBypass || !virtualBypass {
		t.Fatal("F_NOCACHE was not applied to both cold-read paths")
	}
	if coldRatio < 0.70 {
		t.Fatalf("managed cold throughput ratio %.3f is below 0.70", coldRatio)
	}
	if warmRatio < 0.80 {
		t.Fatalf("managed warm throughput ratio %.3f is below 0.80", warmRatio)
	}
}

func TestNativeFSKitMountedManagedCacheSurvivesUnrelatedNamespaceChange(t *testing.T) {
	virtualPath := os.Getenv(nativeFSKitVirtualFileEnv)
	referencePath := os.Getenv(nativeFSKitVirtualReferenceFileEnv)
	if virtualPath == "" || referencePath == "" {
		t.Skipf("set %s and %s to run managed cache coherency", nativeFSKitVirtualFileEnv, nativeFSKitVirtualReferenceFileEnv)
	}
	root := nativeFSKitMountedTestRoot(t)
	// Let setup namespace changes drain before establishing the cache baseline.
	time.Sleep(750 * time.Millisecond)
	for index := 0; index < 2; index++ {
		if _, _, err := sequentialReadMetric(virtualPath, false); err != nil {
			t.Fatal(err)
		}
	}
	writeMountedTestFile(t, filepath.Join(root, "unrelated.bin"), []byte("unrelated namespace change\n"))
	time.Sleep(750 * time.Millisecond)

	nativeWarm, err := medianSequentialThroughput(referencePath, 3)
	if err != nil {
		t.Fatal(err)
	}
	virtualWarm, _, err := sequentialReadMetric(virtualPath, false)
	if err != nil {
		t.Fatal(err)
	}
	ratio := virtualWarm / nativeWarm
	t.Logf(
		"native-fskit managed cache after unrelated namespace change native=%.2fMiB/s virtual=%.2fMiB/s ratio=%.3f",
		nativeWarm/(1<<20), virtualWarm/(1<<20), ratio,
	)
	if ratio < 0.80 {
		t.Fatalf("unrelated namespace change reduced managed warm throughput ratio to %.3f", ratio)
	}
}

func uncachedReferenceCopy(sourcePath string) (string, bool, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", false, fmt.Errorf("open cold reference source: %w", err)
	}
	defer source.Close()
	sourceBypass := false
	if _, err := unix.FcntlInt(source.Fd(), unix.F_NOCACHE, 1); err == nil {
		sourceBypass = true
	}

	destination, err := os.CreateTemp(filepath.Dir(sourcePath), ".codexfold-cold-reference-*.jsonl")
	if err != nil {
		return "", false, fmt.Errorf("create cold reference copy: %w", err)
	}
	destinationPath := destination.Name()
	keep := false
	defer func() {
		_ = destination.Close()
		if !keep {
			_ = os.Remove(destinationPath)
		}
	}()
	destinationBypass := false
	if _, err := unix.FcntlInt(destination.Fd(), unix.F_NOCACHE, 1); err == nil {
		destinationBypass = true
	}

	buffer := make([]byte, 4<<20)
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			written := 0
			for written < count {
				amount, writeErr := destination.Write(buffer[written:count])
				if writeErr != nil {
					return "", sourceBypass && destinationBypass, fmt.Errorf("write cold reference copy: %w", writeErr)
				}
				if amount == 0 {
					return "", sourceBypass && destinationBypass, io.ErrShortWrite
				}
				written += amount
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", sourceBypass && destinationBypass, fmt.Errorf("read cold reference source: %w", readErr)
		}
		if count == 0 {
			return "", sourceBypass && destinationBypass, io.ErrNoProgress
		}
	}
	if err := destination.Sync(); err != nil {
		return "", sourceBypass && destinationBypass, fmt.Errorf("sync cold reference copy: %w", err)
	}
	if err := destination.Close(); err != nil {
		return "", sourceBypass && destinationBypass, fmt.Errorf("close cold reference copy: %w", err)
	}
	keep = true
	return destinationPath, sourceBypass && destinationBypass, nil
}

func writePerformanceFixture(path string, size int64) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	buffer := bytes.Repeat([]byte("{\"codexfold_performance\":true}\n"), 1<<15)
	var written int64
	for written < size {
		chunk := buffer
		if remaining := size - written; int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		n, writeErr := file.Write(chunk)
		written += int64(n)
		if writeErr != nil {
			_ = file.Close()
			return writeErr
		}
	}
	return errors.Join(file.Sync(), file.Close())
}

func sequentialReadMetric(path string, bypassCache bool) (float64, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	bypassApplied := false
	if bypassCache {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_NOCACHE, 1); err == nil {
			bypassApplied = true
		}
	}
	// io.CopyBuffer would let io.Discard.ReadFrom ignore this buffer and turn
	// the bulk metric into thousands of tiny FSKit calls.
	buffer := make([]byte, 4<<20)
	started := time.Now()
	var read int64
	for {
		count, readErr := file.Read(buffer)
		read += int64(count)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, bypassApplied, readErr
		}
		if count == 0 {
			return 0, bypassApplied, io.ErrNoProgress
		}
	}
	duration := time.Since(started)
	if duration <= 0 {
		return 0, bypassApplied, errors.New("sequential read duration is unavailable")
	}
	return float64(read) / duration.Seconds(), bypassApplied, nil
}

func medianSequentialThroughput(path string, runs int) (float64, error) {
	values := make([]float64, 0, runs)
	for index := 0; index < runs; index++ {
		value, _, err := sequentialReadMetric(path, false)
		if err != nil {
			return 0, err
		}
		values = append(values, value)
	}
	sort.Float64s(values)
	return values[len(values)/2], nil
}

func streamingSHA256(path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.CopyBuffer(digest, file, make([]byte, 4<<20)); err != nil {
		return [sha256.Size]byte{}, err
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func durationPercentile(values []time.Duration, percentile float64) time.Duration {
	ordered := append([]time.Duration(nil), values...)
	slices.Sort(ordered)
	index := int(float64(len(ordered)-1) * percentile)
	return ordered[index]
}

func waitForMountedSize(t *testing.T, path string, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var size int64
	var lastErr error
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil {
			size = info.Size()
			if size == want {
				return
			}
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mounted size %s = %d err=%v, want %d", path, size, lastErr, want)
}

func nativeFSKitMountedTestRoot(t *testing.T) string {
	t.Helper()
	mountPoint := nativeFSKitMountPoint(t)
	base := filepath.Join(mountPoint, "sessions", "2099", "12", "31")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("create mounted test base: %v", err)
	}
	root, err := os.MkdirTemp(base, "native-fskit-integration-")
	if err != nil {
		t.Fatalf("create mounted test root: %v", err)
	}
	relativeRoot, err := filepath.Rel(mountPoint, root)
	if err != nil {
		t.Fatalf("resolve mounted test root: %v", err)
	}
	nativeRoot := os.Getenv(nativeFSKitNativeRootEnv)
	nativePath := ""
	if nativeRoot != "" {
		if !filepath.IsAbs(nativeRoot) {
			t.Fatalf("%s must be absolute", nativeFSKitNativeRootEnv)
		}
		nativePath = filepath.Join(nativeRoot, relativeRoot)
	}
	t.Cleanup(func() {
		var cleanupErr error
		if err := os.RemoveAll(root); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("mounted path: %w", err))
		}
		if nativePath != "" {
			if err := os.RemoveAll(nativePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("native path: %w", err))
			}
		}
		if cleanupErr != nil {
			t.Errorf("cleanup mounted test root: %v", cleanupErr)
		}
	})
	return root
}

func assertOpenFileRange(t *testing.T, file *os.File, want []byte, offset int64, length int) {
	t.Helper()
	buffer := make([]byte, length)
	n, err := file.ReadAt(buffer, offset)
	if err != nil {
		t.Fatalf("read offset=%d length=%d: %v", offset, length, err)
	}
	if n != length || !bytes.Equal(buffer, want[offset:offset+int64(length)]) {
		t.Fatalf("range offset=%d length=%d differs", offset, length)
	}
}

func waitForOpenFileRange(t *testing.T, file *os.File, want []byte, offset int64, length int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []byte
	var lastErr error
	for time.Now().Before(deadline) {
		last = make([]byte, length)
		var n int
		n, lastErr = file.ReadAt(last, offset)
		if lastErr == nil && n == length && bytes.Equal(last, want[offset:offset+int64(length)]) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("open file range offset=%d length=%d remained stale: err=%v bytes=%x", offset, length, lastErr, last)
}

func nativeFSKitMountPoint(t *testing.T) string {
	t.Helper()
	mountPoint := os.Getenv(nativeFSKitMountEnv)
	if mountPoint == "" {
		t.Skipf("set %s to run native FSKit mount tests", nativeFSKitMountEnv)
	}
	if !filepath.IsAbs(mountPoint) {
		t.Fatalf("%s must be absolute", nativeFSKitMountEnv)
	}
	return mountPoint
}

func writeMountedTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertMountedTestContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content %s = %q, want %q", path, got, want)
	}
}

func assertNativeFSKitBackingContent(t *testing.T, mountedPath string, want []byte) {
	t.Helper()
	mountPoint := nativeFSKitMountPoint(t)
	nativeRoot := os.Getenv(nativeFSKitNativeRootEnv)
	if nativeRoot == "" {
		t.Skipf("set %s to verify native backing content", nativeFSKitNativeRootEnv)
	}
	relative, err := filepath.Rel(mountPoint, mountedPath)
	if err != nil {
		t.Fatalf("resolve native backing path: %v", err)
	}
	path := filepath.Join(nativeRoot, relative)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native backing %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("native backing %s = %q, want %q", path, got, want)
	}
}

func mountedTestXattr(t *testing.T, path string, attribute string) []byte {
	t.Helper()
	value, err := readMountedTestXattr(path, attribute)
	if err != nil {
		t.Fatalf("get xattr: %v", err)
	}
	return value
}

func readMountedTestXattr(path string, attribute string) ([]byte, error) {
	size, err := unix.Getxattr(path, attribute, nil)
	if err != nil {
		return nil, err
	}
	value := make([]byte, size)
	n, err := unix.Getxattr(path, attribute, value)
	return value[:n], err
}

func mountedTestXattrNames(t *testing.T, path string) []string {
	t.Helper()
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		t.Fatalf("size xattrs: %v", err)
	}
	buffer := make([]byte, size)
	n, err := unix.Listxattr(path, buffer)
	if err != nil {
		t.Fatalf("list xattrs: %v", err)
	}
	buffer = buffer[:n]
	var names []string
	for len(buffer) > 0 {
		index := bytes.IndexByte(buffer, 0)
		if index < 0 {
			t.Fatalf("malformed xattr name list %q", buffer)
		}
		names = append(names, string(buffer[:index]))
		buffer = buffer[index+1:]
	}
	return names
}

func isNotSupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

func waitForMountedContent(t *testing.T, path string, want []byte, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []byte
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = os.ReadFile(path)
		if lastErr == nil && bytes.Equal(last, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mounted content %s = %q err=%v, want %q", path, last, lastErr, want)
}

func waitForMountedAbsence(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, lastErr = os.Stat(path)
		if errors.Is(lastErr, os.ErrNotExist) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mounted path %s remained visible: %v", path, lastErr)
}

func nativeFSKitMountedNamespaceVersion(t *testing.T) (uint64, bool) {
	t.Helper()
	resource := os.Getenv(nativeFSKitResourceEnv)
	if resource == "" {
		return 0, false
	}
	client, err := fskitproto.DialResource(resource, time.Second)
	if err != nil {
		t.Fatalf("dial native FSKit resource: %v", err)
	}
	defer client.Close()
	payload, err := client.Call(fskitproto.OpNamespaceVersion, nil)
	if err != nil {
		t.Fatalf("read native FSKit namespace version: %v", err)
	}
	decoder := fskitproto.NewDecoder(payload)
	version, err := decoder.Uint64()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("decode native FSKit namespace version: %v", err)
	}
	return version, true
}

func logNativeFSKitMountedEntry(t *testing.T, stage string, path string) {
	t.Helper()
	resource := os.Getenv(nativeFSKitResourceEnv)
	if resource == "" {
		return
	}
	if filepath.IsAbs(path) && !canonicalNamespacePath(path) {
		relative, err := filepath.Rel(nativeFSKitMountPoint(t), path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatalf("resolve native FSKit entry route %s: %v", path, err)
		}
		path = "/" + filepath.ToSlash(relative)
	}
	client, err := fskitproto.DialResource(resource, time.Second)
	if err != nil {
		t.Fatalf("dial native FSKit resource: %v", err)
	}
	defer client.Close()
	request := fskitproto.NewEncoder(len(path) + 8)
	request.String(path)
	payload, err := client.Call(fskitproto.OpGetattr, request.Data())
	if err != nil {
		t.Fatalf("get native FSKit entry %s: %v", path, err)
	}
	decoder := fskitproto.NewDecoder(payload)
	entry, err := decoder.Entry()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("decode native FSKit entry %s: %v", path, err)
	}
	t.Logf(
		"%s path=%s node=%d size=%d namespace=%d mtime=%d.%09d ctime=%d.%09d",
		stage, path, entry.NodeID, entry.Size, entry.NamespaceID,
		entry.ModTime.Unix(), entry.ModTime.Nanosecond(),
		entry.ChangeTime.Unix(), entry.ChangeTime.Nanosecond(),
	)
}

func fileInfoSize(info os.FileInfo) int64 {
	if info == nil {
		return -1
	}
	return info.Size()
}

func waitForNativeFSKitNamespaceAdvance(t *testing.T, previous uint64, timeout time.Duration) uint64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		version, _ := nativeFSKitMountedNamespaceVersion(t)
		if version > previous {
			return version
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("native FSKit namespace version did not advance beyond %d", previous)
	return previous
}
