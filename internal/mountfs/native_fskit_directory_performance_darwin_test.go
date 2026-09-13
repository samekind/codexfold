//go:build darwin

package mountfs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This is a real-mount measurement with correctness assertions, not a latency
// acceptance gate: PASS establishes enumeration correctness, not acceptable UX.
func TestNativeFSKitMountedDirectoryEnumerationPerformance(t *testing.T) {
	nativeRoot := os.Getenv(nativeFSKitNativeRootEnv)
	if nativeRoot == "" {
		t.Skipf("set %s and %s to measure a real FSKit mount", nativeFSKitNativeRootEnv, nativeFSKitMountEnv)
	}
	mountPoint := nativeFSKitMountPoint(t)
	root := nativeFSKitMountedTestRoot(t)
	relative, err := filepath.Rel(mountPoint, root)
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(nativeRoot, relative)
	const count = 3000
	mtime := time.Unix(1_720_000_100, 0)
	payload := func(index int) []byte {
		return []byte(fmt.Sprintf("{\"directory_performance_fixture\":%04d}\n", index))
	}
	for index := 0; index < count; index++ {
		name := filepath.Join(native, fmt.Sprintf("entry-%04d.jsonl", index))
		if err := os.WriteFile(name, payload(index), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	type enumerationTiming struct{ total, readDir, stat time.Duration }
	enumerate := func(directory string) enumerationTiming {
		t.Helper()
		started := time.Now()
		entries, err := os.ReadDir(directory)
		readDirTime := time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != count {
			t.Fatalf("%s entries=%d, want=%d", directory, len(entries), count)
		}
		statStarted := time.Now()
		for index, entry := range entries {
			wantName := fmt.Sprintf("entry-%04d.jsonl", index)
			if entry.Name() != wantName {
				t.Fatalf("entry[%d]=%q, want=%q", index, entry.Name(), wantName)
			}
			// Explicit Stat, rather than DirEntry.Info, models callers that stat
			// every discovered session and cannot reuse readdir attributes.
			info, err := os.Stat(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != int64(len(payload(index))) || !info.ModTime().Equal(mtime) {
				t.Fatalf("incorrect metadata for %s: mode=%v size=%d mtime=%v", entry.Name(), info.Mode(), info.Size(), info.ModTime())
			}
		}
		return enumerationTiming{total: time.Since(started), readDir: readDirTime, stat: time.Since(statStarted)}
	}
	var nativeTimes, mountedTimes []time.Duration
	for round := 0; round < 5; round++ {
		var nativeTime, mountedTime enumerationTiming
		if round%2 == 0 {
			nativeTime, mountedTime = enumerate(native), enumerate(root)
		} else {
			mountedTime, nativeTime = enumerate(root), enumerate(native)
		}
		nativeTimes = append(nativeTimes, nativeTime.total)
		mountedTimes = append(mountedTimes, mountedTime.total)
		t.Logf("directory enumeration round=%d files=%d native=%s mounted=%s mounted_over_native=%.3fx native_readdir=%s mounted_readdir=%s native_stat=%s mounted_stat=%s", round+1, count, nativeTime.total, mountedTime.total, float64(mountedTime.total)/float64(nativeTime.total), nativeTime.readDir, mountedTime.readDir, nativeTime.stat, mountedTime.stat)
	}
	// Content reads are deliberately outside the enumeration timer.
	started := time.Now()
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("entry-%04d.jsonl", index)
		for _, directory := range []string{native, root} {
			got, err := os.ReadFile(filepath.Join(directory, name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload(index)) {
				t.Fatalf("content mismatch for %s", filepath.Join(directory, name))
			}
		}
	}
	nativeMedian, mountedMedian := durationPercentile(nativeTimes, 0.5), durationPercentile(mountedTimes, 0.5)
	t.Logf("directory enumeration summary files=%d rounds=5 native_median=%s mounted_median=%s mounted_over_native=%.3fx native_max=%s mounted_max=%s all_content_validation=%s; PASS means correctness only, timings require independent UX assessment", count, nativeMedian, mountedMedian, float64(mountedMedian)/float64(nativeMedian), durationPercentile(nativeTimes, 1), durationPercentile(mountedTimes, 1), time.Since(started))
}
