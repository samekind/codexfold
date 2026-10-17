package mountfs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// heapGarbage mimics the bytes found in a real rollout after a full disk:
// binary memory containing newlines, carriage returns and NULs.
var heapGarbage = []byte("\xf6\x00\x91PV\x0731\x00\x00y\x8c\r\n\x00\x00\x008Ua\xad%\x18\x00\x00\n\x00\x00flow.state\x00\xe7\x8b\n")

func repairFixtureRecords(first int, count int) []byte {
	var out bytes.Buffer
	for index := first; index < first+count; index++ {
		out.WriteString(`{"ordinal":` + string(rune('0'+index%10)) + `}` + "\n")
	}
	return out.Bytes()
}

func readQuarantine(t *testing.T, directory string) nativeQuarantineManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest nativeQuarantineManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func assertNoRepairTemporaries(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && isNativeRepairTemporary(entry.Name()) {
			t.Errorf("repair left temporary file %s", path)
		}
		return nil
	})
}

func TestNativeRepairFailureRetainsQuarantinedBytes(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory write permissions")
	}
	original := append(append([]byte(nil), repairFixtureRecords(0, 2)...), heapGarbage...)
	root, path := nativePreflightFixture(t, original)
	directory := filepath.Dir(path)
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(directory, 0o700)
	repair, changed, err := repairNativeRollout(context.Background(), root, path, 0)
	if err == nil || changed || repair.QuarantinePath == "" {
		t.Fatalf("failed replacement lost recovery location: changed=%t err=%v repair=%+v", changed, err, repair)
	}
	manifest := readQuarantine(t, repair.QuarantinePath)
	if len(manifest.Ranges) != 1 {
		t.Fatalf("quarantine ranges: %+v", manifest.Ranges)
	}
	removed, err := os.ReadFile(filepath.Join(repair.QuarantinePath, manifest.Ranges[0].File))
	if err != nil || !bytes.Equal(removed, heapGarbage) {
		t.Fatalf("quarantined bytes changed: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("original changed before failed replacement: %v", err)
	}
}

func TestNativePreflightRepairsInteriorGarbageLosslessly(t *testing.T) {
	good := repairFixtureRecords(0, 5)
	tail := repairFixtureRecords(5, 4)
	original := append(append(append([]byte(nil), good...), heapGarbage...), tail...)
	root, path := nativePreflightFixture(t, original)
	modTime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}

	report, err := preflightNativeWriterRollouts(context.Background(), root, true)
	if err != nil || len(report.Issues) != 0 {
		t.Fatalf("repair preflight err=%v report=%#v", err, report)
	}
	if len(report.Repairs) != 1 || report.Repairs[0].RemovedBytes != int64(len(heapGarbage)) {
		t.Fatalf("repairs = %#v", report.Repairs)
	}
	repaired, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]byte(nil), good...), tail...); !bytes.Equal(repaired, want) {
		t.Fatalf("repaired content = %q", repaired)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 || !info.ModTime().Equal(modTime) {
		t.Fatalf("repair changed mode/mtime: %v %v", info.Mode().Perm(), info.ModTime())
	}
	if _, err := ValidateNativeRollout(context.Background(), path); err != nil {
		t.Fatalf("repaired rollout is still invalid: %v", err)
	}

	// The quarantine is enough to rebuild the original byte for byte.
	manifest := readQuarantine(t, report.Repairs[0].QuarantinePath)
	if manifest.OriginalSize != int64(len(original)) || len(manifest.Ranges) != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}
	span := manifest.Ranges[0]
	removed, err := os.ReadFile(filepath.Join(report.Repairs[0].QuarantinePath, span.File))
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := append(append(append([]byte(nil), repaired[:span.Start]...), removed...), repaired[span.Start:]...)
	if !bytes.Equal(rebuilt, original) {
		t.Fatal("quarantine does not reproduce the original rollout")
	}
	assertNoRepairTemporaries(t, root)

	second, err := preflightNativeWriterRollouts(context.Background(), root, true)
	if err != nil || len(second.Repairs) != 0 || second.CachedFiles != 1 {
		t.Fatalf("second preflight err=%v report=%#v", err, second)
	}
}

func TestNativePreflightRepairsSeveralDamagedRanges(t *testing.T) {
	original := bytes.Join([][]byte{
		repairFixtureRecords(0, 2), []byte("not-json\n"), repairFixtureRecords(2, 3),
		{0xff, 0xfe, '\n'}, []byte("{broken\n"), repairFixtureRecords(5, 2),
	}, nil)
	root, path := nativePreflightFixture(t, original)
	report, err := preflightNativeWriterRollouts(context.Background(), root, true)
	if err != nil || len(report.Issues) != 0 || len(report.Repairs) != 1 || report.Repairs[0].RemovedRanges != 2 {
		t.Fatalf("repair err=%v report=%#v", err, report)
	}
	want := bytes.Join([][]byte{repairFixtureRecords(0, 2), repairFixtureRecords(2, 3), repairFixtureRecords(5, 2)}, nil)
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, want) {
		t.Fatalf("repaired content = %q", got)
	}
}

func TestNativePreflightRepairsTornTail(t *testing.T) {
	root, path := nativePreflightFixture(t, append(repairFixtureRecords(0, 3), []byte(`{"ordinal":`)...))
	report, err := preflightNativeWriterRollouts(context.Background(), root, true)
	if err != nil || len(report.Issues) != 0 || len(report.Repairs) != 1 {
		t.Fatalf("repair err=%v report=%#v", err, report)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, repairFixtureRecords(0, 3)) {
		t.Fatalf("torn tail not removed: %q", got)
	}
}

func TestNativePreflightFinishesCompleteRecordMissingNewline(t *testing.T) {
	root, path := nativePreflightFixture(t, append(repairFixtureRecords(0, 2), []byte(`{"ordinal":7}`)...))
	report, err := preflightNativeWriterRollouts(context.Background(), root, true)
	if err != nil || len(report.Repairs) != 1 || !report.Repairs[0].AddedFinalLF || report.Repairs[0].QuarantinePath != "" {
		t.Fatalf("repair err=%v report=%#v", err, report)
	}
	want := append(repairFixtureRecords(0, 2), []byte("{\"ordinal\":7}\n")...)
	if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
		t.Fatalf("content = %q", got)
	}
}

func TestNativePreflightRepairsGarbageAppendedAfterCachedPrefix(t *testing.T) {
	prefix := repairFixtureRecords(0, 4)
	root, path := nativePreflightFixture(t, prefix)
	if _, err := preflightNativeWriterRollouts(context.Background(), root, true); err != nil {
		t.Fatal(err)
	}
	appended := append(append(append([]byte(nil), heapGarbage...), repairFixtureRecords(4, 2)...), []byte("tail-garbage\n")...)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(appended); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	report, err := preflightNativeWriterRollouts(context.Background(), root, true)
	if err != nil || len(report.Issues) != 0 || len(report.Repairs) != 1 || report.IncrementalFiles != 1 {
		t.Fatalf("repair err=%v report=%#v", err, report)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, append(append([]byte(nil), prefix...), repairFixtureRecords(4, 2)...)) {
		t.Fatalf("content = %q", got)
	}
	if report.Bytes != int64(len(prefix)+len(repairFixtureRecords(4, 2))) {
		t.Fatalf("report bytes = %d", report.Bytes)
	}
}

func TestNativePreflightRepairModeNeverBlocksOnUnfixableEntries(t *testing.T) {
	root, _ := nativePreflightFixture(t, repairFixtureRecords(0, 2))
	dir := filepath.Join(root, "sessions", "2026", "07", "16")
	if err := os.Symlink(filepath.Join(dir, "rollout-fixture.jsonl"), filepath.Join(dir, "rollout-link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := preflightNativeWriterRollouts(context.Background(), root, false); err == nil {
		t.Fatal("strict preflight accepted a symlink")
	}
	report, err := preflightNativeWriterRollouts(context.Background(), root, true)
	if err != nil {
		t.Fatalf("repair preflight blocked: %v", err)
	}
	if len(report.Issues) != 1 || !strings.Contains(report.Issues[0].Message, "symlink") || report.ValidatedFiles != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestNativePreflightRepairRemovesStaleTemporaries(t *testing.T) {
	root, path := nativePreflightFixture(t, repairFixtureRecords(0, 2))
	stale := filepath.Join(filepath.Dir(path), nativeRepairTempPrefix+"123"+nativeRepairTempSuffix)
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := preflightNativeWriterRollouts(context.Background(), root, true); err != nil {
		t.Fatal(err)
	}
	assertNoRepairTemporaries(t, root)
}

func TestNativeStrictPreflightStillRejectsDamage(t *testing.T) {
	root, path := nativePreflightFixture(t, append(repairFixtureRecords(0, 2), heapGarbage...))
	before, _ := os.ReadFile(path)
	if _, err := preflightNativeWriterRollouts(context.Background(), root, false); err == nil {
		t.Fatal("strict preflight accepted damage")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("strict preflight modified the rollout")
	}
}
