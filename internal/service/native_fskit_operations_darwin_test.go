//go:build darwin

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestProbeNativeFSKitMountStateTreatsMissingMountPointAsUnmounted(t *testing.T) {
	state, err := probeNativeFSKitMountState(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("missing mount point probe: %v", err)
	}
	if state.Mounted || state.Owned || state.Healthy {
		t.Fatalf("missing mount point state = %#v", state)
	}
}

func TestProbeNativeFSKitMountStateDoesNotStatThroughAnOrdinaryDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	state, err := probeNativeFSKitMountState(context.Background(), dir)
	if err != nil {
		t.Fatalf("ordinary directory probe: %v", err)
	}
	if state.Mounted || state.Owned || state.Healthy {
		t.Fatalf("ordinary directory treated as a mount: %#v", state)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("mount-table probe blocked for %s", elapsed)
	}
}

func TestLookupNativeFSKitMountStateSeesRootAsMountedForeign(t *testing.T) {
	state, err := lookupNativeFSKitMountState("/")
	if err != nil {
		t.Fatalf("lookup /: %v", err)
	}
	if !state.Mounted {
		t.Fatal("kernel mount table omitted /")
	}
	if state.Owned || state.Healthy {
		t.Fatalf("root must not look like a CodexFold volume: %#v", state)
	}
}

func nativeFSKitTestMountEntry(path, filesystem string) unix.Statfs_t {
	var stat unix.Statfs_t
	copy(stat.Mntonname[:], path)
	copy(stat.Fstypename[:], filesystem)
	return stat
}

func TestNativeFSKitMountTableExactMatchDoesNotResolveAnyPath(t *testing.T) {
	for _, filesystem := range []string{"codexfold", "apfs"} {
		t.Run(filesystem, func(t *testing.T) {
			stats := []unix.Statfs_t{
				nativeFSKitTestMountEntry("/Volumes/unresponsive", "nfs"),
				nativeFSKitTestMountEntry("/Users/test/.codex/fold-fs", filesystem),
			}
			var resolved []string
			state := nativeFSKitMountStateFromTable("/Users/test/.codex/fold-fs/", stats, func(path string) string {
				resolved = append(resolved, path)
				return filepath.Clean(path)
			})
			if len(resolved) != 0 {
				t.Fatalf("mount-table-only probe performed filesystem path resolution: %v", resolved)
			}
			owned := filesystem == "codexfold"
			if !state.Mounted || state.Owned != owned || state.Healthy != owned {
				t.Fatalf("mount state = %#v", state)
			}
		})
	}
}

func TestNativeFSKitMountTableAliasResolvesOnlyRequestedPath(t *testing.T) {
	stats := []unix.Statfs_t{
		nativeFSKitTestMountEntry("/Volumes/unresponsive", "nfs"),
		nativeFSKitTestMountEntry("/private/tmp/fold-fs", "codexfold"),
	}
	var resolved []string
	state := nativeFSKitMountStateFromTable("/tmp/fold-fs", stats, func(path string) string {
		resolved = append(resolved, path)
		if path == "/tmp/fold-fs" {
			return "/private/tmp/fold-fs"
		}
		return filepath.Clean(path)
	})
	if !reflect.DeepEqual(resolved, []string{"/tmp/fold-fs"}) {
		t.Fatalf("alias lookup resolved paths outside the requested alias: %v", resolved)
	}
	if !state.Mounted || !state.Owned || !state.Healthy {
		t.Fatalf("alias mount state = %#v", state)
	}
}

func TestNativeFSKitMountTableEmptyDoesNotResolvePath(t *testing.T) {
	state := nativeFSKitMountStateFromTable("/missing", nil, func(path string) string {
		t.Fatalf("empty mount table unexpectedly resolved %q", path)
		return path
	})
	if state.Mounted || state.Owned || state.Healthy {
		t.Fatalf("empty table mount state = %#v", state)
	}
}

func TestLookupNativeFSKitMountStateAcceptsSymlinkToForeignMount(t *testing.T) {
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink("/", alias); err != nil {
		t.Fatal(err)
	}
	state, err := lookupNativeFSKitMountState(alias)
	if err != nil {
		t.Fatalf("lookup root alias: %v", err)
	}
	if !state.Mounted || state.Owned || state.Healthy {
		t.Fatalf("root alias must remain a foreign mount: %#v", state)
	}
}

func TestNativeFSKitMountTableDoesNotTreatParentOrPrefixAsMount(t *testing.T) {
	stats := []unix.Statfs_t{nativeFSKitTestMountEntry("/Users/test/.codex/fold-fs", "codexfold")}
	for _, path := range []string{"/Users/test/.codex", "/Users/test/.codex/fold-fs/sessions", "/Users/test/.codex/fold-fs-other"} {
		t.Run(path, func(t *testing.T) {
			state := nativeFSKitMountStateFromTable(path, stats, filepath.Clean)
			if state.Mounted || state.Owned || state.Healthy {
				t.Fatalf("non-root path accepted as mount: %#v", state)
			}
		})
	}
}

func TestNativeFSKitMountArgumentsForceFSKitModule(t *testing.T) {
	want := []string{"-F", "-t", "codexfoldnative", "/tmp/resource", "/tmp/mount"}
	if got := nativeFSKitMountArguments(NativeFSKitMountType, "/tmp/resource", "/tmp/mount"); !reflect.DeepEqual(got, want) {
		t.Fatalf("mount arguments = %v, want %v", got, want)
	}
}

func TestNativeFSKitMountArgumentsSelectAcceptanceModule(t *testing.T) {
	want := []string{"-F", "-t", "codexfolda108", "/tmp/resource", "/tmp/mount"}
	if got := nativeFSKitMountArguments("codexfolda108", "/tmp/resource", "/tmp/mount"); !reflect.DeepEqual(got, want) {
		t.Fatalf("mount arguments = %v, want %v", got, want)
	}
}

func TestNativeFSKitMountErrorExplainsDisabledModuleRecovery(t *testing.T) {
	err := nativeFSKitMountError(errors.New("exit status 69"), []byte("Module vip.jstar.codexfold.fskitprofileprobe.module is disabled!\nmount: Unable to invoke task"))
	if !strings.Contains(err.Error(), "System Settings > General > Login Items & Extensions") {
		t.Fatalf("disabled module error omitted the actionable system setting: %v", err)
	}
	if !strings.Contains(err.Error(), "CodexFoldFSKit FSKit Modules") {
		t.Fatalf("disabled module error omitted the extension name: %v", err)
	}
}

func TestBoundedNativeFSKitMountStateTimesOutWithoutStartingConcurrentHungProbes(t *testing.T) {
	probeSlot := make(chan struct{}, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	probe := func(context.Context) (NativeFSKitMountState, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true}, nil
	}

	start := time.Now()
	_, err := boundedNativeFSKitMountState(context.Background(), 20*time.Millisecond, probeSlot, probe)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first bounded probe error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded probe returned after %s", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("bounded probe did not start the mount inspection")
	}

	secondStarted := time.Now()
	_, err = boundedNativeFSKitMountState(context.Background(), 20*time.Millisecond, probeSlot, probe)
	if !errors.Is(err, ErrNativeFSKitMountProbeInProgress) {
		t.Fatalf("second bounded probe error = %v, want probe-in-progress", err)
	}
	if elapsed := time.Since(secondStarted); elapsed > 10*time.Millisecond {
		t.Fatalf("concurrent probe waited behind the occupied slot for %s", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent hung mount probes = %d, want 1", got)
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for len(probeSlot) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	state, err := boundedNativeFSKitMountState(context.Background(), time.Second, probeSlot, probe)
	if err != nil {
		t.Fatalf("probe after hung inspection completed: %v", err)
	}
	if !state.Mounted || !state.Owned || !state.Healthy {
		t.Fatalf("probe state after recovery = %#v", state)
	}
}
