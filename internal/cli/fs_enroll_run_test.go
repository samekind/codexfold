package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/service"
)

func TestEnrollmentWorkerRequiresPausedFreshBuiltin(t *testing.T) {
	store := t.TempDir()
	now := time.Now()
	control := enroll.Control{Enabled: false, Interval: time.Minute, StableFor: time.Hour, BatchSize: 1}
	if err := requireBuiltinEnrollmentPaused(store, now); err == nil {
		t.Fatal("absent policy accepted")
	}
	if err := enroll.SaveControl(enroll.ControlPath(store), control); err != nil {
		t.Fatal(err)
	}
	for _, p := range []enroll.Progress{
		{Enabled: true, Phase: enroll.PhaseFolding, UpdatedAt: now},
		{Enabled: false, Phase: enroll.PhaseDisabled, UpdatedAt: now.Add(-time.Minute)},
	} {
		if err := enroll.SaveProgress(enroll.ProgressPath(store), p); err != nil {
			t.Fatal(err)
		}
		if err := requireBuiltinEnrollmentPaused(store, now); err == nil {
			t.Fatal("active or stale acknowledgement accepted")
		}
	}
	if err := enroll.SaveProgress(enroll.ProgressPath(store), enroll.Progress{Phase: enroll.PhaseDisabled, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := requireBuiltinEnrollmentPaused(store, now); err != nil {
		t.Fatal(err)
	}
	control.Enabled = true
	if err := enroll.SaveControl(enroll.ControlPath(store), control); err != nil {
		t.Fatal(err)
	}
	if err := requireBuiltinEnrollmentPaused(store, now); err == nil {
		t.Fatal("re-enabled built-in loop accepted")
	}
}

func TestEnrollmentWorkerResumesAfterStaleHeartbeat(t *testing.T) {
	store := t.TempDir()
	control := enroll.Control{Interval: time.Minute, StableFor: time.Hour, BatchSize: 1}
	if err := enroll.SaveControl(enroll.ControlPath(store), control); err != nil {
		t.Fatal(err)
	}
	writeHeartbeat := func(at time.Time) {
		t.Helper()
		if err := enroll.SaveProgress(enroll.ProgressPath(store), enroll.Progress{Phase: enroll.PhaseDisabled, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	writeHeartbeat(time.Now())
	control.Enabled = true
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), control); err != nil {
		t.Fatal(err)
	}
	oldCycle, oldPoll := runServiceEnrollmentCycle, enrollmentPolicyPollInterval
	t.Cleanup(func() { runServiceEnrollmentCycle, enrollmentPolicyPollInterval = oldCycle, oldPoll })
	enrollmentPolicyPollInterval = 10 * time.Millisecond
	started := make(chan int32, 4)
	canceled := make(chan struct{}, 1)
	var cycles atomic.Int32
	runServiceEnrollmentCycle = func(ctx context.Context, _ enrollmentFlags, _ enrollmentApplyHooks) (FSEnrollmentApplyResult, error) {
		n := cycles.Add(1)
		started <- n
		if n == 1 {
			<-ctx.Done()
			canceled <- struct{}{}
			return FSEnrollmentApplyResult{}, ctx.Err()
		}
		return FSEnrollmentApplyResult{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	command := newFSEnrollRunCommand()
	command.SetArgs([]string{"--apply", "--canonical-namespace", "--codex-home", store, "--store", store})
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- command.ExecuteContext(ctx) }()
	defer func() { cancel(); <-exited }()
	awaitCycle := func(want int32) {
		t.Helper()
		select {
		case n := <-started:
			if n != want {
				t.Fatalf("cycle = %d, want %d", n, want)
			}
		case err := <-done:
			t.Fatalf("worker exited before recovery: %v", err)
		case <-ctx.Done():
			t.Fatal("worker did not resume")
		}
	}
	awaitPaused := func() {
		t.Helper()
		for {
			progress, err := enroll.LoadProgress(enroll.WorkerProgressPath(store))
			if err == nil && progress.Enabled && progress.Phase == enroll.PhaseWaitingFilesystem && progress.ErrorKind == "filesystem" {
				break
			}
			select {
			case err := <-done:
				t.Fatalf("worker exited on stale heartbeat: %v", err)
			case <-ctx.Done():
				t.Fatal("worker did not pause")
			case <-time.After(10 * time.Millisecond):
			}
		}
		select {
		case err := <-done:
			t.Fatalf("worker exited while paused: %v", err)
		case <-time.After(5 * enrollmentPolicyPollInterval):
		}
	}
	awaitCycle(1)
	writeHeartbeat(time.Now().Add(-time.Minute))
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("active cycle was not canceled")
	}
	awaitPaused()
	writeHeartbeat(time.Now())
	awaitCycle(2)
	// Also recover when sleep interrupts the idle interval between cycles.
	writeHeartbeat(time.Now().Add(-time.Minute))
	awaitPaused()
	writeHeartbeat(time.Now())
	awaitCycle(3)
	// An explicit stop still works even if the filesystem heartbeat is stale.
	writeHeartbeat(time.Now().Add(-time.Minute))
	control.Enabled = false
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), control); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("explicit stop did not terminate worker")
	}
}

func TestPersistentEnrollmentWorkerWaitsAndKeepsUserPolicy(t *testing.T) {
	store := t.TempDir()
	policy := enroll.Control{Enabled: true, Interval: time.Minute, StableFor: time.Hour, BatchSize: 5}
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), policy); err != nil {
		t.Fatal(err)
	}
	oldCycle, oldPoll := runServiceEnrollmentCycle, enrollmentPolicyPollInterval
	t.Cleanup(func() { runServiceEnrollmentCycle, enrollmentPolicyPollInterval = oldCycle, oldPoll })
	enrollmentPolicyPollInterval = 10 * time.Millisecond
	cycles := make(chan enrollmentFlags, 4)
	runServiceEnrollmentCycle = func(_ context.Context, flags enrollmentFlags, _ enrollmentApplyHooks) (FSEnrollmentApplyResult, error) {
		select {
		case cycles <- flags:
		default:
		}
		return FSEnrollmentApplyResult{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runEnrollmentWorker(ctx, enrollmentFlags{codexHome: store, storeDir: store, canonicalNamespace: true}, true)
	}()
	defer func() { cancel(); <-exited }()
	awaitPhase := func(phase string, enabled bool) {
		t.Helper()
		for {
			p, err := enroll.LoadProgress(enroll.WorkerProgressPath(store))
			if err == nil && p.Phase == phase && p.Enabled == enabled {
				return
			}
			select {
			case err := <-done:
				t.Fatalf("persistent worker exited: %v", err)
			case <-ctx.Done():
				t.Fatalf("did not reach %s: %#v %v", phase, p, err)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	awaitCycle := func() {
		t.Helper()
		select {
		case flags := <-cycles:
			if flags.batchSize != 5 || flags.stableFor != time.Hour {
				t.Fatalf("lost user policy: %#v", flags)
			}
		case <-ctx.Done():
			t.Fatal("worker did not resume after readiness")
		}
	}
	// No startup heartbeat: the service publishes an accurate waiting state
	// and remains alive. It must never fabricate an acknowledgement.
	awaitPhase(enroll.PhaseWaitingFilesystem, true)
	select {
	case <-cycles:
		t.Fatal("cycle started before filesystem readiness")
	default:
	}
	if err := enroll.SaveControl(enroll.ControlPath(store), enroll.Control{Interval: time.Minute, StableFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveProgress(enroll.ProgressPath(store), enroll.Progress{Phase: enroll.PhaseDisabled, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	awaitCycle()
	// User pause keeps the service available for subsequent hot re-enable.
	policy.Enabled = false
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), policy); err != nil {
		t.Fatal(err)
	}
	awaitPhase(enroll.PhaseDisabled, false)
	if err := os.WriteFile(enroll.WorkerControlPath(store), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	awaitPhase(enroll.PhaseConfigInvalid, false)
	policy.Enabled = true
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), policy); err != nil {
		t.Fatal(err)
	}
	awaitCycle()
	before, err := os.ReadFile(enroll.WorkerControlPath(store))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("SCM cancellation did not stop worker")
	}
	after, err := os.ReadFile(enroll.WorkerControlPath(store))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("service shutdown changed persisted user intent")
	}
	p, err := enroll.LoadProgress(enroll.WorkerProgressPath(store))
	if err != nil || !p.Enabled || p.Phase != enroll.PhaseStopped {
		t.Fatalf("shutdown status = %#v, %v", p, err)
	}
}

func TestEnrollmentWorkerPolicyStopsCycleAndKeepsBuiltinProgress(t *testing.T) {
	store := t.TempDir()
	control := enroll.Control{Interval: time.Minute, StableFor: time.Hour, BatchSize: 1}
	if err := enroll.SaveControl(enroll.ControlPath(store), control); err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveProgress(enroll.ProgressPath(store), enroll.Progress{Phase: enroll.PhaseDisabled, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(enroll.ProgressPath(store))
	if err != nil {
		t.Fatal(err)
	}
	control.Enabled = true
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), control); err != nil {
		t.Fatal(err)
	}
	oldCycle, oldPoll := runServiceEnrollmentCycle, enrollmentPolicyPollInterval
	t.Cleanup(func() { runServiceEnrollmentCycle, enrollmentPolicyPollInterval = oldCycle, oldPoll })
	enrollmentPolicyPollInterval = 10 * time.Millisecond
	started := make(chan struct{})
	runServiceEnrollmentCycle = func(ctx context.Context, _ enrollmentFlags, _ enrollmentApplyHooks) (FSEnrollmentApplyResult, error) {
		close(started)
		<-ctx.Done()
		return FSEnrollmentApplyResult{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := newFSEnrollRunCommand()
	command.SetArgs([]string{"--apply", "--canonical-namespace", "--codex-home", store, "--store", store})
	done := make(chan error, 1)
	go func() { done <- command.ExecuteContext(ctx) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("worker did not start")
	}
	if lock, err := service.AcquireProcessLock(filepath.Join(store, "enrollment", "worker.lock")); err == nil {
		lock.Close()
		t.Fatal("duplicate worker lock accepted")
	}
	control.Enabled = false
	if err := enroll.SaveControl(enroll.WorkerControlPath(store), control); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("worker did not stop")
	}
	after, err := os.ReadFile(enroll.ProgressPath(store))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("worker overwrote built-in progress")
	}
	progress, err := enroll.LoadProgress(enroll.WorkerProgressPath(store))
	if err != nil {
		t.Fatal(err)
	}
	if progress.Enabled || progress.Phase != enroll.PhaseDisabled {
		t.Fatalf("worker did not acknowledge stop: %#v", progress)
	}
}
