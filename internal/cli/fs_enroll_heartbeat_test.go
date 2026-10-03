package cli

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
)

func TestPeriodicEnrollmentHeartbeatsDuringLongOperation(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "builtin"
		if external {
			name = "worker"
		}
		t.Run(name, func(t *testing.T) {
			oldCycle, oldPoll := runServiceEnrollmentCycle, enrollmentPolicyPollInterval
			t.Cleanup(func() { runServiceEnrollmentCycle, enrollmentPolicyPollInterval = oldCycle, oldPoll })
			enrollmentPolicyPollInterval = 10 * time.Millisecond
			store := t.TempDir()
			flags := enrollmentFlags{storeDir: store, externalWorker: external}
			policyPath, progressPath := enroll.ControlPath(store), enroll.ProgressPath(store)
			var builtinBefore []byte
			if external {
				if err := enroll.SaveControl(policyPath, enroll.Control{Interval: time.Minute, StableFor: time.Hour}); err != nil {
					t.Fatal(err)
				}
				if err := enroll.SaveProgress(progressPath, enroll.Progress{Phase: enroll.PhaseDisabled}); err != nil {
					t.Fatal(err)
				}
				var err error
				builtinBefore, err = os.ReadFile(progressPath)
				if err != nil {
					t.Fatal(err)
				}
				policyPath, progressPath = enroll.WorkerControlPath(store), enroll.WorkerProgressPath(store)
			}
			if err := enroll.SaveControl(policyPath, enroll.Control{Enabled: true, Interval: time.Minute, StableFor: time.Hour}); err != nil {
				t.Fatal(err)
			}
			started, advance := make(chan struct{}), make(chan struct{})
			runServiceEnrollmentCycle = func(ctx context.Context, _ enrollmentFlags, hooks enrollmentApplyHooks) (FSEnrollmentApplyResult, error) {
				hooks.onManagedCount(124)
				hooks.onPhase(enroll.PhaseReclaiming)
				hooks.onProgress(18, 20)
				close(started)
				select {
				case <-advance:
					hooks.onManagedCount(125)
					hooks.onProgress(19, 20)
				case <-ctx.Done():
					return FSEnrollmentApplyResult{}, ctx.Err()
				}
				<-ctx.Done()
				return FSEnrollmentApplyResult{}, ctx.Err()
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { runPeriodicEnrollment(ctx, flags, time.Minute, nil); close(done) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("enrollment loop did not stop")
				}
			})
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("long operation did not start")
			}
			first, err := enroll.LoadProgress(progressPath)
			if err != nil {
				t.Fatal(err)
			}
			waitForHeartbeat := func(after time.Time, completed, managed int) enroll.Progress {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					p, err := enroll.LoadProgress(progressPath)
					if err == nil && p.UpdatedAt.After(after) && p.CycleDone == completed && p.ManagedCount == managed {
						if !p.Enabled || p.Phase != enroll.PhaseReclaiming || p.CycleTotal != 20 {
							t.Fatalf("heartbeat changed active operation: %#v", p)
						}
						return p
					}
					time.Sleep(enrollmentPolicyPollInterval)
				}
				t.Fatal("progress timestamp did not advance during long operation")
				return enroll.Progress{}
			}
			second := waitForHeartbeat(first.UpdatedAt, 18, 124)
			third := waitForHeartbeat(second.UpdatedAt, 18, 124)
			close(advance)
			fourth := waitForHeartbeat(third.UpdatedAt, 19, 125)
			waitForHeartbeat(fourth.UpdatedAt, 19, 125)
			if external {
				after, err := os.ReadFile(enroll.ProgressPath(store))
				if err != nil || !bytes.Equal(builtinBefore, after) {
					t.Fatal("worker heartbeat overwrote filesystem progress")
				}
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("active cycle did not stop")
			}
			stopped, err := os.ReadFile(progressPath)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * enrollmentPolicyPollInterval)
			after, err := os.ReadFile(progressPath)
			if err != nil || !bytes.Equal(stopped, after) {
				t.Fatal("heartbeat continued after loop stopped")
			}
		})
	}
}
