package mountfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestNativeAppendGapFailsClosedAndCanRetry(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "gap")
	handle := openNativeAppendTestHandle(t, filesystem, route)
	defer filesystem.Release(handle)

	record := []byte("{\"record\":1}\n")
	if n, errno := filesystem.Write(handle, record, int64(len(base)+5)); errno != 0 || n != len(record) {
		t.Fatalf("stage gapped append: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(handle); errno != 0 {
		t.Fatalf("intermediate gapped fsync errno=%v", errno)
	}
	if errno := filesystem.Flush(handle); errno != syscall.EIO {
		t.Fatalf("final gapped flush errno=%v, want EIO", errno)
	}
	assertNativeBytes(t, nativePath, base)

	if n, errno := filesystem.Write(handle, record, int64(len(base))); errno != 0 || n != len(record) {
		t.Fatalf("stage retry: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(handle); errno != 0 {
		t.Fatalf("valid retry fsync: %v", errno)
	}
	assertNativeBytes(t, nativePath, append(append([]byte(nil), base...), record...))
}

func TestNativeGetattrDoesNotHideExternalGrowthBehindIdleAppendState(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "external-growth")
	handle, errno := filesystem.Open(route, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open native reader: %v", errno)
	}
	defer filesystem.Release(handle)

	tail := []byte("{\"record\":1}\n")
	file, err := os.OpenFile(nativePath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(tail); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	attribute, errno := filesystem.Getattr(route)
	want := int64(len(base) + len(tail))
	if errno != 0 || attribute.Size != want {
		t.Fatalf("Getattr after external growth size=%d errno=%v, want %d", attribute.Size, errno, want)
	}
}

func TestNativeAppendIntermediateFsyncKeepsPartialRecord(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "partial-fsync")
	handle := openNativeAppendTestHandle(t, filesystem, route)
	defer filesystem.Release(handle)
	record := append([]byte("{\"payload\":\""), bytes.Repeat([]byte("x"), 96*1024)...)
	record = append(record, []byte("\"}\n")...)
	split := 32 * 1024
	if n, errno := filesystem.Write(handle, record[:split], int64(len(base))); errno != 0 || n != split {
		t.Fatalf("stage partial record: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(handle); errno != 0 {
		t.Fatalf("intermediate fsync: %v", errno)
	}
	assertNativeBytes(t, nativePath, base)
	if n, errno := filesystem.Write(handle, record[split:], int64(len(base)+split)); errno != 0 || n != len(record)-split {
		t.Fatalf("finish record: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(handle); errno != 0 {
		t.Fatalf("complete fsync: %v", errno)
	}
	assertNativeBytes(t, nativePath, append(append([]byte(nil), base...), record...))
}

func TestNativeAppendEarlyHandleReleaseDoesNotDiscardOtherWriter(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "multi-handle-release")
	largeHandle := openNativeAppendTestHandle(t, filesystem, route)
	smallHandle := openNativeAppendTestHandle(t, filesystem, route)
	large := append([]byte("{\"large\":\""), bytes.Repeat([]byte("x"), 96*1024)...)
	large = append(large, []byte("\"}\n")...)
	small := []byte("{\"small\":true}\n")
	split := 32 * 1024
	if n, errno := filesystem.Write(largeHandle, large[:split], int64(len(base))); errno != 0 || n != split {
		t.Fatalf("stage large prefix: n=%d errno=%v", n, errno)
	}
	if n, errno := filesystem.Write(smallHandle, small, int64(len(base)+len(large))); errno != 0 || n != len(small) {
		t.Fatalf("stage later record: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Release(smallHandle); errno != 0 {
		t.Fatalf("release later writer: %v", errno)
	}
	assertNativeBytes(t, nativePath, base)
	if n, errno := filesystem.Write(largeHandle, large[split:], int64(len(base)+split)); errno != 0 || n != len(large)-split {
		t.Fatalf("finish large record: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Release(largeHandle); errno != 0 {
		t.Fatalf("release final writer: %v", errno)
	}
	want := append(append(append([]byte(nil), base...), large...), small...)
	assertNativeBytes(t, nativePath, want)
}

func TestNativeAppendConflictingOverlapFailsImmediately(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "conflict")
	handle := openNativeAppendTestHandle(t, filesystem, route)
	defer filesystem.Release(handle)

	record := []byte("{\"record\":1}\n")
	if n, errno := filesystem.Write(handle, record, int64(len(base))); errno != 0 || n != len(record) {
		t.Fatalf("stage record: n=%d errno=%v", n, errno)
	}
	if n, errno := filesystem.Write(handle, []byte("X"), int64(len(base)+2)); errno != syscall.EIO || n != 0 {
		t.Fatalf("conflicting overlap: n=%d errno=%v, want n=0 EIO", n, errno)
	}
	attribute, errno := filesystem.Getattr(route)
	if errno != 0 || attribute.Size != int64(len(base)) {
		t.Fatalf("conflict remained visible: size=%d errno=%v", attribute.Size, errno)
	}
	assertNativeBytes(t, nativePath, base)

	if n, errno := filesystem.Write(handle, record, int64(len(base))); errno != 0 || n != len(record) {
		t.Fatalf("stage valid retry: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(handle); errno != 0 {
		t.Fatalf("valid retry fsync: %v", errno)
	}
}

func TestNativeAppendIdenticalRetriesStayDeduplicated(t *testing.T) {
	filesystem, route, _, base := nativeAppendTestFilesystem(t, "deduplicated")
	handle := openNativeAppendTestHandle(t, filesystem, route)
	defer filesystem.Release(handle)
	record := []byte("{\"record\":1}\n")
	for index := 0; index < 10_000; index++ {
		if n, errno := filesystem.Write(handle, record, int64(len(base))); errno != 0 || n != len(record) {
			t.Fatalf("retry %d: n=%d errno=%v", index, n, errno)
		}
	}
	state := filesystem.handles[handle].nativeAppend
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.segments) != 1 || len(state.segments[0].data) != len(record) {
		t.Fatalf("duplicate retries retained: segments=%d bytes=%d", len(state.segments), len(state.segments[0].data))
	}
}

func TestNativeAppendFlushCommits(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "flush")
	handle := openNativeAppendTestHandle(t, filesystem, route)
	defer filesystem.Release(handle)
	record := []byte("{\"flush\":true}\n")
	if n, errno := filesystem.Write(handle, record, int64(len(base))); errno != 0 || n != len(record) {
		t.Fatalf("stage append: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Flush(handle); errno != 0 {
		t.Fatalf("flush: %v", errno)
	}
	assertNativeBytes(t, nativePath, append(append([]byte(nil), base...), record...))
}

func TestNativeAppendReleaseCommits(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "release")
	handle := openNativeAppendTestHandle(t, filesystem, route)
	record := []byte("{\"release\":true}\n")
	if n, errno := filesystem.Write(handle, record, int64(len(base))); errno != 0 || n != len(record) {
		t.Fatalf("stage append: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Release(handle); errno != 0 {
		t.Fatalf("release: %v", errno)
	}
	assertNativeBytes(t, nativePath, append(append([]byte(nil), base...), record...))
}

func TestNativeAppendTruncateFailsWhilePending(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "truncate")
	handle := openNativeAppendTestHandle(t, filesystem, route)
	defer filesystem.Release(handle)
	record := []byte("{\"pending\":true}\n")
	if n, errno := filesystem.Write(handle, record, int64(len(base))); errno != 0 || n != len(record) {
		t.Fatalf("stage append: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Truncate(handle, 0); errno != syscall.EIO {
		t.Fatalf("handle truncate errno=%v, want EIO", errno)
	}
	if errno := filesystem.TruncatePath(route, 0); errno != syscall.EIO {
		t.Fatalf("path truncate errno=%v, want EIO", errno)
	}
	assertNativeBytes(t, nativePath, base)
	if errno := filesystem.Fsync(handle); errno != 0 {
		t.Fatalf("commit after rejected truncate: %v", errno)
	}
}

func TestNativeAppendRejectsInvalidUTF8(t *testing.T) {
	filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, "utf8")
	handle := openNativeAppendTestHandle(t, filesystem, route)
	defer filesystem.Release(handle)
	invalid := []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n'}
	if n, errno := filesystem.Write(handle, invalid, int64(len(base))); errno != 0 || n != len(invalid) {
		t.Fatalf("stage invalid UTF-8: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(handle); errno != syscall.EIO {
		t.Fatalf("invalid UTF-8 fsync errno=%v, want EIO", errno)
	}
	assertNativeBytes(t, nativePath, base)
}

func TestNativeAppendHandlesLargeOutOfOrderRecords(t *testing.T) {
	for _, payloadBytes := range []int{32*1024 + 1, 64*1024 + 1, 1 << 20} {
		t.Run(fmt.Sprintf("payload-%d", payloadBytes), func(t *testing.T) {
			filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, fmt.Sprintf("large-%d", payloadBytes))
			handle := openNativeAppendTestHandle(t, filesystem, route)
			defer filesystem.Release(handle)
			record := append([]byte("{\"payload\":\""), bytes.Repeat([]byte("x"), payloadBytes)...)
			record = append(record, []byte("\"}\n")...)
			const chunkBytes = 31 * 1024
			for end := len(record); end > 0; {
				start := max(0, end-chunkBytes)
				offset := int64(len(base) + start)
				if n, errno := filesystem.Write(handle, record[start:end], offset); errno != 0 || n != end-start {
					t.Fatalf("stage chunk [%d:%d]: n=%d errno=%v", start, end, n, errno)
				}
				end = start
			}
			if errno := filesystem.Fsync(handle); errno != 0 {
				t.Fatalf("commit large record: %v", errno)
			}
			assertNativeBytes(t, nativePath, append(append([]byte(nil), base...), record...))
		})
	}
}

func TestNativeAppendConcurrentSessionsRemainIndependent(t *testing.T) {
	const sessionCount = 8
	type fixture struct {
		filesystem *Filesystem
		route      string
		path       string
		base       []byte
		handle     uint64
		tail       []byte
	}
	fixtures := make([]fixture, 0, sessionCount)
	for session := 0; session < sessionCount; session++ {
		filesystem, route, nativePath, base := nativeAppendTestFilesystem(t, fmt.Sprintf("parallel-%d", session))
		handle := openNativeAppendTestHandle(t, filesystem, route)
		t.Cleanup(func() { _ = filesystem.Release(handle) })
		var tail []byte
		for record := 0; record < 200; record++ {
			tail = append(tail, fmt.Appendf(nil, "{\"session\":%d,\"record\":%d}\n", session, record)...)
		}
		fixtures = append(fixtures, fixture{filesystem: filesystem, route: route, path: nativePath, base: base, handle: handle, tail: tail})
	}

	var group sync.WaitGroup
	errors := make(chan error, sessionCount)
	for _, item := range fixtures {
		item := item
		group.Add(1)
		go func() {
			defer group.Done()
			if n, errno := item.filesystem.Write(item.handle, item.tail, int64(len(item.base))); errno != 0 || n != len(item.tail) {
				errors <- fmt.Errorf("write %s: n=%d errno=%v", item.route, n, errno)
				return
			}
			if errno := item.filesystem.Fsync(item.handle); errno != 0 {
				errors <- fmt.Errorf("fsync %s: %v", item.route, errno)
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	for _, item := range fixtures {
		assertNativeBytes(t, item.path, append(append([]byte(nil), item.base...), item.tail...))
	}
}

func TestRecoverNativeAppendTransactionRollsBackPartialCommit(t *testing.T) {
	nativeRoot, journalRoot, nativePath, base, tail, record := nativeAppendRecoveryFixture(t, "partial")
	if _, err := writeNativeAppendJournal(journalRoot, record); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(nativePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(tail[:len(tail)/2], int64(len(base))); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recoverNativeAppendTransactions(nativeRoot, journalRoot); err != nil {
		t.Fatal(err)
	}
	assertNativeBytes(t, nativePath, base)
	assertEmptyJournal(t, journalRoot)
}

func TestRecoverNativeAppendTransactionKeepsVerifiedCommit(t *testing.T) {
	nativeRoot, journalRoot, nativePath, base, tail, record := nativeAppendRecoveryFixture(t, "committed")
	if _, err := writeNativeAppendJournal(journalRoot, record); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(nativePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(tail, int64(len(base))); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recoverNativeAppendTransactions(nativeRoot, journalRoot); err != nil {
		t.Fatal(err)
	}
	assertNativeBytes(t, nativePath, append(append([]byte(nil), base...), tail...))
	assertEmptyJournal(t, journalRoot)
}

func TestRecoverNativeAppendTransactionRejectsOutsideTarget(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	journalRoot := filepath.Join(nativeRoot, ".codexfold-native-journal")
	outside := filepath.Join(root, "outside.jsonl")
	if err := os.MkdirAll(nativeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("{\"outside\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("{\"tail\":true}\n"))
	record := nativeAppendJournal{
		Version: nativeAppendJournalVersion, TargetPath: outside,
		BaseSize: int64(len("{\"outside\":true}\n")), FinalSize: int64(len("{\"outside\":true}\n{\"tail\":true}\n")),
		TailSHA256: hex.EncodeToString(digest[:]),
	}
	if _, err := writeNativeAppendJournal(journalRoot, record); err != nil {
		t.Fatal(err)
	}
	if err := recoverNativeAppendTransactions(nativeRoot, journalRoot); err == nil {
		t.Fatal("outside journal target was accepted")
	}
	assertNativeBytes(t, outside, []byte("{\"outside\":true}\n"))
}

func nativeAppendTestFilesystem(t *testing.T, name string) (*Filesystem, string, string, []byte) {
	t.Helper()
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := "/sessions/2026/07/16/rollout-" + name + ".jsonl"
	nativePath := nativePathFromRoot(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":0}\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	return filesystem, route, nativePath, base
}

func openNativeAppendTestHandle(t *testing.T, filesystem *Filesystem, route string) uint64 {
	t.Helper()
	handle, errno := filesystem.Open(route, os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("open native append handle: %v", errno)
	}
	return handle
}

func nativeAppendRecoveryFixture(t *testing.T, name string) (string, string, string, []byte, []byte, nativeAppendJournal) {
	t.Helper()
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	journalRoot := filepath.Join(nativeRoot, ".codexfold-native-journal")
	nativePath := filepath.Join(nativeRoot, "sessions", "2026", "07", "16", "rollout-"+name+".jsonl")
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":0}\n")
	tail := []byte("{\"record\":1}\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(tail)
	record := nativeAppendJournal{
		Version: nativeAppendJournalVersion, TargetPath: nativePath,
		BaseSize: int64(len(base)), FinalSize: int64(len(base) + len(tail)),
		TailSHA256: hex.EncodeToString(digest[:]),
	}
	return nativeRoot, journalRoot, nativePath, base, tail, record
}

func assertNativeBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("native bytes at %s = %q err=%v, want %q", path, got, err, want)
	}
}

func assertEmptyJournal(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("journal entries remained: %v", entries)
	}
}
