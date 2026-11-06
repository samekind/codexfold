package nativezst

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codexzstd"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
)

func requireSupport(t *testing.T) {
	t.Helper()
	if !supported() || !codexzstd.Supported() {
		t.Skip("folded .zst files need cgo on darwin or linux")
	}
}

// rolloutContent looks like a forked rollout: a long unique history replayed
// further back than zstd's 2 MiB window at level 3, which per-file zstd
// cannot share and content-defined chunking can.
func rolloutContent(seed int64, lines int) []byte {
	random := rand.New(rand.NewSource(seed))
	var history bytes.Buffer
	for line := 0; line < lines || history.Len() < 2400<<10; line++ {
		payload := make([]byte, 100+random.Intn(3000))
		for i := range payload {
			payload[i] = "abcdefghij klmnop"[random.Intn(17)]
		}
		fmt.Fprintf(&history, `{"ordinal":%d,"text":%q}`+"\n", line, payload)
	}
	return append(append([]byte(nil), history.Bytes()...), history.Bytes()...)
}

// writeCodexZst writes what Codex's compression worker leaves behind.
func writeCodexZst(t *testing.T, path string, content []byte, modTime time.Time) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := codexzstd.Encode(context.Background(), &encoded, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func readAllAt(t *testing.T, reader *Reader, chunk int) []byte {
	t.Helper()
	var out bytes.Buffer
	buffer := make([]byte, chunk)
	for offset := int64(0); ; {
		n, err := reader.ReadAt(buffer, offset)
		out.Write(buffer[:n])
		offset += int64(n)
		if err == io.EOF || n == 0 {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return out.Bytes()
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	store := filepath.Join(root, "fold-store")
	native := filepath.Join(root, "fold-native")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	return store, native
}

func TestFoldConvertServeAndRestore(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	path := filepath.Join(native, "sessions", "2026", "09", "01", "rollout-2026-09-01T10-00-00-0199aaaa-0000-7000-8000-000000000001.jsonl.zst")
	modTime := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	content := rolloutContent(1, 300)
	original := writeCodexZst(t, path, content, modTime)
	if err := setTestXattr(path, "user.example.kept", []byte("value")); err != nil {
		t.Fatal(err)
	}

	source := NewSource(store, testPacks{})
	defer source.Close()
	prepared, err := Prepare(context.Background(), source, path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Lstat(path)
	if err := Convert(store, prepared); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Size() != int64(len(original)) || !after.ModTime().Equal(modTime) || after.Mode().Perm() != 0o640 {
		t.Fatalf("conversion changed identity: size=%d mtime=%v mode=%v", after.Size(), after.ModTime(), after.Mode())
	}
	if !dataReleased(after) {
		t.Fatalf("converted file still allocates %d bytes", allocatedBytes(after))
	}
	if value := make([]byte, 16); true {
		n, err := getTestXattr(path, "user.example.kept", value)
		if err != nil || string(value[:n]) != "value" {
			t.Fatalf("unrelated xattr lost: %q %v", value[:n], err)
		}
	}
	marker, folded, err := ReadMarker(path)
	if err != nil || !folded || marker != prepared.Marker {
		t.Fatalf("marker = %#v %t %v", marker, folded, err)
	}

	reader, err := source.Open(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllAt(t, reader, 64<<10); !bytes.Equal(got, original) {
		t.Fatal("served bytes differ from the original .zst")
	}
	// Reads behind and ahead of the encoder cursor.
	for _, offset := range []int64{int64(len(original)) - 100, 10, int64(len(original)) / 2, 0} {
		buffer := make([]byte, 100)
		n, err := reader.ReadAt(buffer, offset)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !bytes.Equal(buffer[:n], original[offset:offset+int64(n)]) {
			t.Fatalf("read at %d differs", offset)
		}
	}
	_ = reader.Close()

	if err := Restore(context.Background(), source, path); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("restore failed: %v", err)
	}
	if _, folded, _ := ReadMarker(path); folded {
		t.Fatal("restore kept the marker")
	}
	if info, _ := os.Lstat(path); !info.ModTime().Equal(modTime) {
		t.Fatalf("restore changed mtime to %v", info.ModTime())
	}
}

func TestServeFromPackAfterLooseRetirement(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	path := filepath.Join(native, "archived_sessions", "rollout-2026-09-02T10-00-00-0199aaaa-0000-7000-8000-000000000002.jsonl.zst")
	original := writeCodexZst(t, path, rolloutContent(2, 200), time.Now().Add(-48*time.Hour))
	source := NewSource(store, testPacks{})
	defer source.Close()
	prepared, err := Prepare(context.Background(), source, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Convert(store, prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.RetireLoose(context.Background(), store, pack.RetireLooseOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	reader, err := source.Open(prepared.Marker)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got := readAllAt(t, reader, 1<<20); !bytes.Equal(got, original) {
		t.Fatal("bytes served from the pack differ")
	}
}

func TestSecondCopyReusesStoredContent(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	content := rolloutContent(3, 200)
	first := filepath.Join(native, "sessions", "rollout-a-0199aaaa-0000-7000-8000-000000000003.jsonl.zst")
	second := filepath.Join(native, "sessions", "rollout-b-0199aaaa-0000-7000-8000-000000000004.jsonl.zst")
	writeCodexZst(t, first, content, time.Now().Add(-time.Hour))
	writeCodexZst(t, second, content, time.Now().Add(-time.Hour))
	source := NewSource(store, testPacks{})
	defer source.Close()
	one, err := Prepare(context.Background(), source, first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Prepare(context.Background(), source, second)
	if err != nil {
		t.Fatal(err)
	}
	if one.NewStoredBytes == 0 || two.NewStoredBytes != 0 {
		t.Fatalf("new stored bytes = %d then %d, want >0 then 0", one.NewStoredBytes, two.NewStoredBytes)
	}
	if one.Marker.Manifest == two.Marker.Manifest {
		t.Fatal("two files share one manifest")
	}
}

func TestPrepareRejectsForeignZstd(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	path := filepath.Join(native, "sessions", "rollout-c-0199aaaa-0000-7000-8000-000000000005.jsonl.zst")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A checksummed frame is valid zstd that Codex never writes.
	if err := os.WriteFile(path, []byte{0x28, 0xb5, 0x2f, 0xfd, 0x24, 0x02, 0x11, 0x00, 0x00, 0x7b, 0x7d, 0x00, 0x00, 0x00, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	source := NewSource(store, testPacks{})
	defer source.Close()
	if _, err := Prepare(context.Background(), source, path); err == nil {
		t.Fatal("foreign zstd file was prepared")
	}
	if entries, _ := os.ReadDir(ManifestRoot(store)); len(entries) != 0 {
		t.Fatalf("rejected file left %d manifests", len(entries))
	}
}

func TestSweepReleasesOnlyAfterTwoMissedScans(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	keep := filepath.Join(native, "sessions", "rollout-keep-0199aaaa-0000-7000-8000-000000000006.jsonl.zst")
	gone := filepath.Join(native, "sessions", "rollout-gone-0199aaaa-0000-7000-8000-000000000007.jsonl.zst")
	writeCodexZst(t, keep, rolloutContent(4, 100), time.Now().Add(-time.Hour))
	writeCodexZst(t, gone, rolloutContent(5, 100), time.Now().Add(-time.Hour))
	source := NewSource(store, testPacks{})
	defer source.Close()
	var prepared []Prepared
	for _, path := range []string{keep, gone} {
		p, err := Prepare(context.Background(), source, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := Convert(store, p); err != nil {
			t.Fatal(err)
		}
		prepared = append(prepared, p)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sweep := func(at time.Time) SweepResult {
		t.Helper()
		scan, err := ScanNative(context.Background(), native)
		if err != nil {
			t.Fatal(err)
		}
		result, err := Sweep(context.Background(), store, scan, at)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := sweep(now); result.Released != 0 || result.Pending != 1 || result.Referenced != 1 {
		t.Fatalf("young manifest sweep = %#v", result)
	}
	later := now.Add(sweepGrace + time.Minute)
	if result := sweep(later); result.Released != 0 {
		t.Fatalf("first missed scan released: %#v", result)
	}
	if result := sweep(later.Add(sweepGrace / 2)); result.Released != 0 {
		t.Fatalf("second scan inside the grace released: %#v", result)
	}
	if result := sweep(later.Add(sweepGrace + time.Minute)); result.Released != 1 || result.Referenced != 1 {
		t.Fatalf("second missed scan = %#v", result)
	}
	if _, err := os.Stat(prepared[1].ManifestPath); !os.IsNotExist(err) {
		t.Fatalf("released manifest still present: %v", err)
	}
	if _, err := os.Stat(prepared[0].ManifestPath); err != nil {
		t.Fatalf("referenced manifest removed: %v", err)
	}
}

func TestSweepRepairsALostMarker(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	path := filepath.Join(native, "sessions", "rollout-lost-0199aaaa-0000-7000-8000-000000000008.jsonl.zst")
	original := writeCodexZst(t, path, rolloutContent(6, 100), time.Now().Add(-time.Hour))
	source := NewSource(store, testPacks{})
	defer source.Close()
	prepared, err := Prepare(context.Background(), source, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Convert(store, prepared); err != nil {
		t.Fatal(err)
	}
	if err := removeTestXattr(path, MarkerAttribute); err != nil {
		t.Fatal(err)
	}
	scan, err := ScanNative(context.Background(), native)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Sweep(context.Background(), store, scan, time.Now().Add(2*sweepGrace))
	if err != nil || result.Repaired != 1 || result.Released != 0 {
		t.Fatalf("repair sweep = %#v, %v", result, err)
	}
	marker, folded, err := ReadMarker(path)
	if err != nil || !folded || marker != prepared.Marker {
		t.Fatalf("repaired marker = %#v %t %v", marker, folded, err)
	}
	reader, err := source.Open(marker)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if !bytes.Equal(readAllAt(t, reader, 1<<20), original) {
		t.Fatal("repaired file serves different bytes")
	}
}

func TestScanFootprint(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	content := rolloutContent(7, 100)
	folded := filepath.Join(native, "sessions", "rollout-f-0199aaaa-0000-7000-8000-000000000009.jsonl.zst")
	plain := filepath.Join(native, "sessions", "rollout-p-0199aaaa-0000-7000-8000-000000000010.jsonl.zst")
	jsonl := filepath.Join(native, "sessions", "rollout-j-0199aaaa-0000-7000-8000-000000000011.jsonl")
	encoded := writeCodexZst(t, folded, content, time.Now().Add(-time.Hour))
	writeCodexZst(t, plain, content, time.Now().Add(-time.Hour))
	if err := os.WriteFile(jsonl, content, 0o644); err != nil {
		t.Fatal(err)
	}
	source := NewSource(store, testPacks{})
	defer source.Close()
	prepared, err := Prepare(context.Background(), source, folded)
	if err != nil {
		t.Fatal(err)
	}
	if err := Convert(store, prepared); err != nil {
		t.Fatal(err)
	}
	scan, err := ScanNative(context.Background(), native)
	if err != nil {
		t.Fatal(err)
	}
	footprint := scan.Footprint
	if footprint.Files != 3 || footprint.FoldedFiles != 1 || footprint.CompressedFiles != 1 || len(scan.Candidates) != 1 {
		t.Fatalf("footprint = %#v", footprint)
	}
	if footprint.LogicalBytes != 3*int64(len(content)) || footprint.CompressedBytes != int64(len(encoded)) {
		t.Fatalf("logical=%d compressed=%d", footprint.LogicalBytes, footprint.CompressedBytes)
	}
}

type testPacks struct{}

func (testPacks) CurrentGeneration(store string) (string, error) {
	return pack.CurrentGeneration(store)
}

func (testPacks) Open(store string) (Pack, error) { return pack.Open(store, pack.OpenOptions{}) }

func TestPrepareStreamsAndPricesTheFoldExactly(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	content := rolloutContent(8, 200)
	first := filepath.Join(native, "sessions", "rollout-t1-0199aaaa-0000-7000-8000-000000000012.jsonl.zst")
	second := filepath.Join(native, "sessions", "rollout-t2-0199aaaa-0000-7000-8000-000000000013.jsonl.zst")
	original := writeCodexZst(t, first, content, time.Now().Add(-time.Hour))
	writeCodexZst(t, second, content, time.Now().Add(-time.Hour))
	source := NewSource(store, testPacks{})
	defer source.Close()
	one, err := Prepare(context.Background(), source, first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Prepare(context.Background(), source, second)
	if err != nil {
		t.Fatal(err)
	}
	if one.NewStoredBytes == 0 || two.NewStoredBytes != 0 {
		t.Fatalf("new stored bytes = %d then %d", one.NewStoredBytes, two.NewStoredBytes)
	}
	if entries, _ := os.ReadDir(filepath.Join(StateRoot(store), "staging")); len(entries) != 0 {
		t.Fatalf("prepare staged %d files", len(entries))
	}
	reader, err := source.Open(two.Marker)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if !bytes.Equal(readAllAt(t, reader, 1<<20), original) {
		t.Fatal("dry-run priced fold serves different bytes")
	}
}

type fixedChecker struct {
	report storage.BudgetReport
	err    error
}

func (c fixedChecker) Check(context.Context, storage.Projection) (storage.Assessment, error) {
	return storage.Assessment{Budget: c.report}, c.err
}

func TestReclaimingGuardDigsOutOnlyWhenItFreesMore(t *testing.T) {
	reserve := fmt.Errorf("%w: fold rejected", storage.ErrBudgetExceeded)
	small := storage.Projection{AdditionalPersistentBytes: 2 << 20, TemporaryBytes: 8 << 20, TemporaryPersistentOverlapBytes: 2 << 20}
	cases := []struct {
		name    string
		report  storage.BudgetReport
		reclaim int64
		allowed bool
	}{
		{"frees more above the floor", storage.BudgetReport{Rejections: []storage.RejectionReason{storage.RejectionFreeSpaceReserve}, FreeSpaceAfterPeak: 2 << 30}, 1 << 30, true},
		{"frees too little", storage.BudgetReport{Rejections: []storage.RejectionReason{storage.RejectionFreeSpaceReserve}, FreeSpaceAfterPeak: 2 << 30}, 16 << 20, false},
		{"below the floor", storage.BudgetReport{Rejections: []storage.RejectionReason{storage.RejectionFreeSpaceReserve}, FreeSpaceAfterPeak: 512 << 20}, 1 << 30, false},
		{"another limit", storage.BudgetReport{Rejections: []storage.RejectionReason{storage.RejectionFreeSpaceReserve, storage.RejectionPhysicalBudget}, FreeSpaceAfterPeak: 2 << 30}, 1 << 30, false},
	}
	for _, tc := range cases {
		guard := reclaimingGuard{inner: fixedChecker{report: tc.report, err: reserve}, reclaim: tc.reclaim}
		_, err := guard.Check(context.Background(), small)
		if (err == nil) != tc.allowed {
			t.Errorf("%s: err = %v, allowed %t", tc.name, err, tc.allowed)
		}
	}
	large := storage.Projection{AdditionalPersistentBytes: 1 << 30}
	guard := reclaimingGuard{inner: fixedChecker{report: cases[0].report, err: reserve}, reclaim: 64 << 30}
	if _, err := guard.Check(context.Background(), large); err == nil {
		t.Error("a large fold ran below the reserve")
	}
}

func TestPrepareKeepsAZstThatFoldingWouldNotShrink(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	random := rand.New(rand.NewSource(9))
	var content bytes.Buffer
	for line := 0; line < 300; line++ {
		payload := make([]byte, 200+random.Intn(800))
		for i := range payload {
			payload[i] = "abcdefghij klmnop"[random.Intn(17)]
		}
		fmt.Fprintf(&content, `{"ordinal":%d,"text":%q}`+"\n", line, payload)
	}
	path := filepath.Join(native, "sessions", "rollout-u-0199aaaa-0000-7000-8000-000000000014.jsonl.zst")
	original := writeCodexZst(t, path, content.Bytes(), time.Now().Add(-time.Hour))
	source := NewSource(store, testPacks{})
	defer source.Close()
	if _, err := Prepare(context.Background(), source, path); !errors.Is(err, ErrNotWorthwhile) {
		t.Fatalf("unique rollout prepare = %v, want ErrNotWorthwhile", err)
	}
	if entries, _ := os.ReadDir(ManifestRoot(store)); len(entries) != 0 {
		t.Fatalf("declined fold left %d manifests", len(entries))
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("declined fold touched the file: %v", err)
	}
}

func TestConcurrentOutOfOrderHandlesShareOneEncoder(t *testing.T) {
	requireSupport(t)
	store, native := fixture(t)
	path := filepath.Join(native, "sessions", "rollout-c-0199aaaa-0000-7000-8000-000000000015.jsonl.zst")
	original := writeCodexZst(t, path, rolloutContent(15, 300), time.Now().Add(-time.Hour))
	source := NewSource(store, testPacks{})
	defer source.Close()
	prepared, err := Prepare(context.Background(), source, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Convert(store, prepared); err != nil {
		t.Fatal(err)
	}
	before := streamsStarted.Load()
	// The FSKit extension prefetches through several handles at once, each
	// taking whichever block is next, so every handle sees jumps both ways.
	const block = 64 << 10
	blocks := (len(original) + block - 1) / block
	order := rand.New(rand.NewSource(3)).Perm(blocks)
	got := make([]byte, len(original))
	var group sync.WaitGroup
	errs := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		reader, err := source.Open(prepared.Marker)
		if err != nil {
			t.Fatal(err)
		}
		group.Add(1)
		go func(reader *Reader, worker int) {
			defer group.Done()
			defer reader.Close()
			for index := worker; index < len(order); index += 4 {
				offset := int64(order[index]) * block
				n, err := reader.ReadAt(got[offset:min(offset+block, int64(len(got)))], offset)
				if err != nil && err != io.EOF {
					errs <- err
					return
				}
				if n == 0 {
					errs <- fmt.Errorf("empty read at %d", offset)
					return
				}
			}
		}(reader, worker)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("concurrent out-of-order reads returned different bytes")
	}
	if started := streamsStarted.Load() - before; started != 1 {
		t.Fatalf("encoders started = %d, want 1", started)
	}
}
