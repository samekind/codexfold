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
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nativeFSKitMountEnv      = "CODEXFOLD_NATIVE_FSKIT_MOUNT"
	nativeFSKitNativeRootEnv = "CODEXFOLD_NATIVE_FSKIT_NATIVE_ROOT"
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
	if err := oldEOFFile.Close(); err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), base...), first...), second...)
	assertMountedTestContent(t, oldEOF, want)
}

func TestNativeFSKitMountedNamespaceOperations(t *testing.T) {
	root := nativeFSKitMountedTestRoot(t)
	source := filepath.Join(root, "source.bin")
	destination := filepath.Join(root, "destination.bin")
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
	target := filepath.Join(root, "open-unlink.bin")
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
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(nativePath) })
	if err := os.WriteFile(nativePath, []byte("external-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForMountedContent(t, mountedPath, []byte("external-one\n"), 3*time.Second)

	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}
	waitForMountedAbsence(t, mountedPath, 3*time.Second)
	if err := os.WriteFile(nativePath, []byte("external-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForMountedContent(t, mountedPath, []byte("external-two\n"), 3*time.Second)
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
	if err := writePerformanceFixture(nativePath, sourceBytes); err != nil {
		t.Fatal(err)
	}
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
	mountedPath := filepath.Join(root, "read-ahead.bin")
	nativePath := filepath.Join(nativeRoot, relativeRoot, "read-ahead.bin")
	const blockSize = 1 << 20
	content := make([]byte, 2*blockSize+257)
	for index := range content {
		content[index] = byte((index*31 + index/251) % 251)
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

	eofOffset := int64(len(content) - 100)
	eofBuffer := make([]byte, 4096)
	n, err := reader.ReadAt(eofBuffer, eofOffset)
	if !errors.Is(err, io.EOF) || n != 100 || !bytes.Equal(eofBuffer[:n], content[eofOffset:]) {
		t.Fatalf("EOF read n=%d err=%v", n, err)
	}

	writeOffset := int64(128 << 10)
	replacement := bytes.Repeat([]byte{0xa5}, 8192)
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
	assertOpenFileRange(t, reader, content, writeOffset, len(replacement))

	truncateSize := int64(blockSize + 123)
	if err := os.Truncate(mountedPath, truncateSize); err != nil {
		t.Fatalf("mounted truncate: %v", err)
	}
	truncated := content[:truncateSize]
	truncateBuffer := make([]byte, 4096)
	n, err = reader.ReadAt(truncateBuffer, truncateSize-100)
	if !errors.Is(err, io.EOF) || n != 100 || !bytes.Equal(truncateBuffer[:n], truncated[truncateSize-100:]) {
		t.Fatalf("post-truncate read n=%d err=%v", n, err)
	}

	externalOffset := int64(64 << 10)
	externalReplacement := bytes.Repeat([]byte{0x3c}, 4096)
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
	waitForOpenFileRange(t, reader, truncated, externalOffset, len(externalReplacement), 3*time.Second)

	renamedPath := filepath.Join(root, "read-ahead-renamed.bin")
	if err := os.Rename(mountedPath, renamedPath); err != nil {
		t.Fatalf("rename cached file: %v", err)
	}
	assertOpenFileRange(t, reader, truncated, 0, 8192)
	assertMountedTestContent(t, renamedPath, truncated)
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
	started := time.Now()
	read, err := io.CopyBuffer(io.Discard, file, make([]byte, 4<<20))
	if err != nil {
		return 0, bypassApplied, err
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
