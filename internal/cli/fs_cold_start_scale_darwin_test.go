//go:build darwin

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/fskitstatus"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/service"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
	_ "modernc.org/sqlite"
)

// Opt-in, read-only with respect to its source. APFS clone files and synthetic
// managed states live under t.TempDir. This measures the per-session cold-open
// work that the one-session acceptance run could not represent.
func TestProductionScaleManagedColdOpen(t *testing.T) {
	source := os.Getenv("CODEXFOLD_SCALE_PACK_SOURCE")
	manifestID := os.Getenv("CODEXFOLD_SCALE_MANIFEST_ID")
	if source == "" || manifestID == "" {
		t.Skip("requires a read-only source pack and one representative manifest ID")
	}
	count := 2186
	if specified := os.Getenv("CODEXFOLD_SCALE_COUNT"); specified != "" {
		parsed, err := strconv.Atoi(specified)
		if err != nil || parsed < 1 || parsed > 5000 {
			t.Fatal("CODEXFOLD_SCALE_COUNT must be 1..5000")
		}
		count = parsed
	}
	source, err := filepath.Abs(source)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(source); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("source store is not a plain directory: %v", err)
	}
	baselineGeneration, err := pack.CurrentGeneration(source)
	if err != nil {
		t.Fatal(err)
	}
	template, err := fold.LoadManifest(source, manifestID)
	if err != nil {
		t.Fatal(err)
	}
	realMount := os.Getenv("CODEXFOLD_SCALE_REAL_MOUNT") == "1"
	mountRemoved := !realMount
	root := ""
	if realMount {
		root, err = os.MkdirTemp("/private/tmp", "cfs-scale-fixture-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if mountRemoved {
				_ = os.RemoveAll(root)
			} else {
				t.Logf("preserving isolated fixture after unmount failure: %s", root)
			}
		})
	} else {
		root = t.TempDir()
	}
	store := filepath.Join(root, "fold-store")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	cloneStarted := time.Now()
	clone := exec.Command("/bin/cp", "-cR", filepath.Join(source, "packs"), store)
	if output, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone source packs: %v: %s", err, output)
	}
	if current, err := pack.CurrentGeneration(source); err != nil || current != baselineGeneration {
		t.Fatalf("source pack changed during clone: before=%s after=%s err=%v", baselineGeneration, current, err)
	}
	if current, err := pack.CurrentGeneration(store); err != nil || current != baselineGeneration {
		t.Fatalf("cloned pack is not the source generation: before=%s after=%s err=%v", baselineGeneration, current, err)
	}
	t.Logf("APFS pack clone elapsed=%s", time.Since(cloneStarted))

	resolver, err := pack.Open(store, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if err := os.MkdirAll(filepath.Join(store, "manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := template
	seed.Session.ID = "scale-seed"
	seed.Session.RolloutPath = filepath.Join(root, "native.jsonl")
	seedBytes, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fold.ManifestPath(store, seed.Session.ID), seedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fold.UnfoldWithOptions(context.Background(), store, seed.Session.ID, fold.UnfoldOptions{
		TargetPath: seed.Session.RolloutPath, Reader: resolver, Budget: scaleAllowBudget{},
	}); err != nil {
		t.Fatalf("materialize one shared synthetic native snapshot: %v", err)
	}
	native := vfs.NativeFile{Path: seed.Session.RolloutPath, Bytes: seed.Source.Bytes, SHA256: seed.Source.SHA256}
	retired := os.Getenv("CODEXFOLD_SCALE_RETIRED") == "1"
	setupStarted := time.Now()
	for index := range count {
		manifest := template
		manifest.Session.ID = fmt.Sprintf("scale-%04d", index)
		manifest.Session.RolloutPath = filepath.Join(root, "archived_sessions", manifest.Session.ID+".jsonl")
		manifest.Session.Archived = true
		if os.Getenv("CODEXFOLD_SCALE_MANIFEST_TAIL") == "1" {
			targetBytes := 10_886
			if index%100 >= 90 {
				targetBytes = 205_091
			}
			if index%100 == 99 {
				targetBytes = 6_746_070
			}
			if padding := targetBytes - len(seedBytes); padding > 0 {
				manifest.Session.Title = strings.Repeat("x", padding)
			}
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := fold.ManifestPath(store, manifest.Session.ID)
		if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot := native
		if retired {
			snapshot.Path = filepath.Join(store, "fs", "snapshots", manifest.Session.ID, "native.jsonl")
			if err := os.MkdirAll(filepath.Dir(snapshot.Path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(native.Path, snapshot.Path); err != nil {
				t.Fatal(err)
			}
		}
		options := vfs.SessionOptions{
			Root: store, ManifestPath: manifestPath, Manifest: manifest,
			Reader: resolver, Budget: scaleAllowBudget{}, NativeSnapshot: snapshot,
		}
		if retired {
			managed, writer, err := vfs.OpenSessionWithWriter(context.Background(), options)
			if err != nil {
				t.Fatalf("publish synthetic state %d: %v", index, err)
			}
			if _, err := managed.RetireNativeSnapshot(snapshot, native); err != nil {
				_ = writer.Close()
				t.Fatalf("retire synthetic snapshot %d: %v", index, err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		} else if _, err := vfs.OpenSession(context.Background(), options); err != nil {
			t.Fatalf("publish synthetic state %d: %v", index, err)
		}
	}
	t.Logf("synthetic managed-state setup count=%d elapsed=%s", count, time.Since(setupStarted))
	states, issues, err := vfs.DiscoverSessionStatesDetailedReadOnly(store)
	if err != nil || len(issues) != 0 || len(states) != count {
		t.Fatalf("synthetic state discovery count=%d issues=%d err=%v", len(states), len(issues), err)
	}
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	opened := make([]*vfs.Session, 0, count)
	readers := make([]*pack.Resolver, 0, count)
	defer func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	}()
	started := time.Now()
	for index, state := range states {
		managed, reader, err := openManagedSessionDeferred(context.Background(), store, state)
		if err != nil {
			t.Fatalf("cold-open session %d: %v", index, err)
		}
		opened = append(opened, managed)
		readers = append(readers, reader)
	}
	elapsed := time.Since(started)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("production-scale deferred cold-open count=%d elapsed=%s heap_before=%d heap_after=%d", len(opened), elapsed, before.HeapAlloc, after.HeapAlloc)
	if binary := os.Getenv("CODEXFOLD_SCALE_SERVE_BIN"); binary != "" {
		measureSyntheticBackendStart(t, binary, root, store, count, &mountRemoved)
	}
}

func measureSyntheticBackendStart(t *testing.T, binary, home, store string, count int, mountRemoved *bool) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table threads (id text primary key, title text, cwd text, rollout_path text, model_provider text, model text, updated_at integer, archived integer, git_branch text)`); err != nil {
		t.Fatal(err)
	}
	transaction, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := transaction.Prepare(`insert into threads values (?, 'scale', '', ?, 'scale', '', 1, 1, '')`)
	if err != nil {
		t.Fatal(err)
	}
	for index := range count {
		id := fmt.Sprintf("scale-%04d", index)
		route := filepath.Join(home, "archived_sessions", id+".jsonl")
		if _, err := statement.Exec(id, route); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("CODEXFOLD_SCALE_PRESEED_ACK") == "1" {
			state, err := vfs.InspectSessionState(filepath.Join(store, "fs", "sessions", id, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := writeMountAcknowledgement(store, id, state.Generation, "/archived_sessions/"+id+".jsonl"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, "fold-native")
	for _, directory := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(native, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realMount := os.Getenv("CODEXFOLD_SCALE_REAL_MOUNT") == "1"
	resourceParent := "/private/tmp"
	if realMount {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		resourceParent = filepath.Join(userHome, "Library", "Group Containers", "group.vip.jstar.codexfold")
	}
	resource, err := os.MkdirTemp(resourceParent, "cfs-scale-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if *mountRemoved {
			_ = os.RemoveAll(resource)
		} else {
			t.Logf("preserving isolated FSKit resource after unmount failure: %s", resource)
		}
	})
	mount := filepath.Join(home, "mount")
	if realMount {
		mount, err = os.MkdirTemp("/private/tmp", "cfs-scale-mount-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Remove(mount); err == nil {
				*mountRemoved = true
			} else {
				t.Logf("preserving isolated mount for recovery: %s (%v)", mount, err)
			}
		})
	}
	statusPath := service.FSKitStatusPath(resource, "daemon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	backendArgs := []string{
		"fs", "serve", "--apply", "--foreground=true",
		"--codex-home", home, "--store", store,
		"--mount", mount,
		"--frontend", "native-fskit", "--canonical-namespace",
		"--native-root", native, "--fskit-resource", resource,
		"--fskit-socket", filepath.Join(resource, "daemon.sock"),
		"--enrollment-interval", "0",
	}
	var output bytes.Buffer
	startBackend := func(path string) (*exec.Cmd, chan error, error) {
		command := exec.CommandContext(ctx, path, backendArgs...)
		command.Env = append(os.Environ(), "CODEXFOLD_STARTUP_PROFILE=1")
		command.Stdout, command.Stderr = io.Discard, &output
		if err := command.Start(); err != nil {
			return nil, nil, err
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		return command, done, nil
	}
	started := time.Now()
	initialBinary := os.Getenv("CODEXFOLD_SCALE_INITIAL_BIN")
	if initialBinary == "" {
		initialBinary = binary
	}
	command, done, err := startBackend(initialBinary)
	if err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		cancel()
		if !exited {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("synthetic backend did not stop after cancellation")
			}
		}
		if realMount {
			t.Logf("replacement backend startup profile:\n%s", output.String())
		}
	}()
	for {
		snapshot, err := fskitstatus.Read(statusPath)
		if err == nil && snapshot.State == "healthy" && snapshot.PID == command.Process.Pid {
			t.Logf("production-scale backend ready count=%d elapsed=%s", count, time.Since(started))
			t.Logf("backend startup profile:\n%s", output.String())
			managed, managedErr := fskitstatus.Read(service.FSKitStatusPath(resource, "managed"))
			if managedErr != nil || managed.State != "healthy" || managed.ManagedSessions == nil || *managed.ManagedSessions != count {
				t.Fatalf("synthetic managed status is not fully loaded: status=%#v err=%v output=%s", managed, managedErr, output.String())
			}
			if idleSeconds := os.Getenv("CODEXFOLD_SCALE_IDLE_SECONDS"); idleSeconds != "" {
				seconds, parseErr := strconv.Atoi(idleSeconds)
				if parseErr != nil || seconds < 1 || seconds > 120 {
					t.Fatal("CODEXFOLD_SCALE_IDLE_SECONDS must be 1..120")
				}
				t.Logf("isolated backend idle observation pid=%d duration=%ds", command.Process.Pid, seconds)
				select {
				case <-time.After(time.Duration(seconds) * time.Second):
				case err := <-done:
					exited = true
					t.Fatalf("isolated backend exited during idle observation: %v", err)
				}
				t.Logf("isolated backend idle profile:\n%s", output.String())
			}
			if realMount {
				restart := func() error {
					handoffStarted := time.Now()
					previousPID := command.Process.Pid
					if err := command.Process.Signal(syscall.SIGTERM); err != nil {
						return err
					}
					select {
					case <-done:
						exited = true
					case <-time.After(10 * time.Second):
						return fmt.Errorf("isolated old backend %d did not stop", previousPID)
					}
					output.Reset()
					newStarted := time.Now()
					command, done, err = startBackend(binary)
					if err != nil {
						return err
					}
					exited = false
					deadline := time.Now().Add(15 * time.Second)
					for time.Now().Before(deadline) {
						current, readErr := fskitstatus.Read(statusPath)
						if readErr == nil && current.State == "healthy" && current.PID == command.Process.Pid {
							t.Logf("isolated replacement ready elapsed=%s total_handoff=%s", time.Since(newStarted), time.Since(handoffStarted))
							return nil
						}
						select {
						case err := <-done:
							exited = true
							return fmt.Errorf("isolated replacement backend exited: %w", err)
						default:
						}
						time.Sleep(25 * time.Millisecond)
					}
					return errors.New("isolated replacement backend did not become healthy")
				}
				verifySyntheticMountedBackend(t, binary, resource, mount, restart)
			}
			return
		}
		select {
		case err := <-done:
			exited = true
			t.Fatalf("synthetic backend exited before readiness: %v output=%s", err, output.String())
		case <-ctx.Done():
			t.Fatalf("synthetic backend did not become ready: %v output=%s", ctx.Err(), output.String())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func verifySyntheticMountedBackend(t *testing.T, binary, resource, mount string, restart func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	supervisor := exec.CommandContext(ctx, binary, "fs", "supervise", "--resource", resource, "--mount", mount,
		"--fskit-type", service.NativeFSKitMountType, "--interval", "250ms", "--probe-timeout", "2s", "--recovery-timeout", "15s", "--apply")
	var output bytes.Buffer
	supervisor.Stdout, supervisor.Stderr = io.Discard, &output
	if err := supervisor.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- supervisor.Wait() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("isolated supervisor did not stop")
		}
		unmountCtx, unmountCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer unmountCancel()
		if err := service.UnmountNativeFSKit(unmountCtx, mount, false); err != nil {
			t.Errorf("unmount isolated scale fixture: %v", err)
		}
	}()
	path := filepath.Join(mount, "archived_sessions", "scale-0000.jsonl")
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(data) != 0 {
			t.Logf("isolated native FSKit mount read managed session bytes=%d", len(data))
			break
		}
		select {
		case err := <-done:
			t.Fatalf("isolated supervisor exited before managed read: %v output=%s", err, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("isolated native mount did not serve managed session: %v output=%s", err, output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if os.Getenv("CODEXFOLD_SCALE_REAL_RESTART") != "1" {
		return
	}
	result := make(chan error, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		result <- restart()
	}()
	type writeReport struct {
		writes           int
		failures         int
		firstErr         error
		longestGap       time.Duration
		longestOperation time.Duration
	}
	stopWriter := make(chan struct{})
	writerDone := make(chan writeReport, 1)
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		var report writeReport
		lastSuccess := time.Now()
		for {
			select {
			case <-stopWriter:
				writerDone <- report
				return
			case <-ticker.C:
			}
			operationStarted := time.Now()
			writer, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err == nil {
				_, err = fmt.Fprintf(writer, "{\"scale_probe\":%d}\n", report.writes)
				if err == nil {
					err = writer.Sync()
				}
				err = errors.Join(err, writer.Close())
			}
			if elapsed := time.Since(operationStarted); elapsed > report.longestOperation {
				report.longestOperation = elapsed
			}
			if err == nil {
				report.writes++
				if gap := time.Since(lastSuccess); gap > report.longestGap {
					report.longestGap = gap
				}
				lastSuccess = time.Now()
			} else {
				report.failures++
				if report.firstErr == nil {
					report.firstErr = err
				}
			}
		}
	}()
	var reads, readFailures int
	var firstReadErr, restartFailure error
	var longestGap, longestReadOperation time.Duration
	lastSuccess := time.Now()
	restarted := false
	finishAt := time.Now().Add(20 * time.Second)
	for time.Now().Before(finishAt) {
		operationStarted := time.Now()
		file, err := os.Open(path)
		if err == nil {
			var prefix [32]byte
			_, err = io.ReadFull(file, prefix[:])
			err = errors.Join(err, file.Close())
		}
		if elapsed := time.Since(operationStarted); elapsed > longestReadOperation {
			longestReadOperation = elapsed
		}
		if err == nil {
			reads++
			if gap := time.Since(lastSuccess); gap > longestGap {
				longestGap = gap
			}
			lastSuccess = time.Now()
		} else {
			readFailures++
			if firstReadErr == nil {
				firstReadErr = err
			}
		}
		select {
		case restartErr := <-result:
			if restartErr != nil {
				restartFailure = restartErr
				finishAt = time.Now()
				break
			}
			restarted = true
			finishAt = time.Now().Add(2 * time.Second)
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	close(stopWriter)
	var written writeReport
	select {
	case written = <-writerDone:
	case <-time.After(15 * time.Second):
		t.Fatal("isolated write probe did not stop")
	}
	t.Logf("isolated real-mount restart continuity: reads=%d writes=%d read_failures=%d write_failures=%d longest_read_gap=%s longest_write_gap=%s longest_read_operation=%s longest_write_operation=%s", reads, written.writes, readFailures, written.failures, longestGap, written.longestGap, longestReadOperation, written.longestOperation)
	if restartFailure != nil {
		t.Fatalf("isolated backend restart failed: %v", restartFailure)
	}
	if !restarted || readFailures != 0 || written.failures != 0 || reads < 20 || written.writes < 5 || longestGap >= 10*time.Second || written.longestGap >= 10*time.Second {
		t.Logf("first read error=%v first write error=%v", firstReadErr, written.firstErr)
		t.Fatal("isolated native FSKit restart did not preserve read/write continuity")
	}
}

type scaleAllowBudget struct{}

func (scaleAllowBudget) Check(context.Context, storage.Projection) (storage.Assessment, error) {
	return storage.Assessment{}, nil
}

// Read-only measurement of the real manifest shape, including large part
// arrays that cannot be represented by the one-source synthetic mount fixture.
func TestProductionManifestParseAndViewCost(t *testing.T) {
	source := os.Getenv("CODEXFOLD_SCALE_PACK_SOURCE")
	if source == "" {
		t.Skip("requires an explicitly selected read-only source store")
	}
	resolver, err := pack.Open(source, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	entries, err := os.ReadDir(filepath.Join(source, "manifests"))
	if err != nil {
		t.Fatal(err)
	}
	var count, parts int
	var bytes, maximumBytes int64
	paths := make([]string, 0, len(entries))
	started := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(source, "manifests", entry.Name())
		paths = append(paths, path)
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := fold.LoadManifestPath(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vfs.NewView(manifest, resolver); err != nil {
			t.Fatal(err)
		}
		count++
		parts += len(manifest.Parts)
		bytes += info.Size()
		if info.Size() > maximumBytes {
			maximumBytes = info.Size()
		}
	}
	t.Logf("real manifest parse and view count=%d bytes=%d parts=%d max_manifest_bytes=%d elapsed=%s", count, bytes, parts, maximumBytes, time.Since(started))
	jobs := make(chan string)
	workerErrors := make(chan error, 1)
	var workers sync.WaitGroup
	parallelStarted := time.Now()
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for path := range jobs {
				manifest, err := fold.LoadManifestPath(path)
				if err == nil {
					_, err = vfs.NewView(manifest, resolver)
				}
				if err != nil {
					select {
					case workerErrors <- fmt.Errorf("%s: %w", path, err):
					default:
					}
				}
			}
		}()
	}
	for _, path := range paths {
		jobs <- path
	}
	close(jobs)
	workers.Wait()
	select {
	case err := <-workerErrors:
		t.Fatal(err)
	default:
	}
	t.Logf("real manifest parse and view parallel_workers=8 count=%d elapsed=%s", len(paths), time.Since(parallelStarted))
}
