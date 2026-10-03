package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/service"
	"github.com/spf13/cobra"
)

// A separately updatable enrollment worker keeps the mounted filesystem alive.
// It only runs after the built-in loop has acknowledged its disabled policy.
func requireBuiltinEnrollmentPaused(store string, now time.Time) error {
	control, err := enroll.LoadControl(enroll.ControlPath(store))
	if err != nil {
		return err
	}
	if !control.Present || control.Enabled {
		return errors.New("pause built-in enrollment before running the separate worker")
	}
	progress, err := enroll.LoadProgress(enroll.ProgressPath(store))
	if err != nil {
		return err
	}
	if progress.Enabled || progress.Phase != enroll.PhaseDisabled || progress.UpdatedAt.IsZero() || now.Sub(progress.UpdatedAt) > 15*time.Second || progress.UpdatedAt.After(now.Add(5*time.Second)) {
		return errors.New("waiting for the running filesystem to acknowledge paused enrollment")
	}
	return nil
}

func newFSEnrollRunCommand() *cobra.Command {
	var flags enrollmentFlags
	var apply bool
	command := &cobra.Command{
		Use: "run", Short: "Run a separately updatable enrollment worker without remounting",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !apply || !flags.canonicalNamespace {
				return errors.New("worker requires --apply and --canonical-namespace")
			}
			return runEnrollmentWorker(command.Context(), flags, false)
		},
	}
	addEnrollmentFlags(command, &flags)
	command.Flags().BoolVar(&apply, "apply", false, "Allow bounded background enrollment")
	return command
}

// The SCM host keeps this loop alive even when the user pauses folding. Both
// startup and later mount outages use the same fail-closed, retrying control
// resolution; there is no one-shot startup deadline to strand the worker.
func runEnrollmentWorker(parent context.Context, flags enrollmentFlags, persistent bool) error {
	store := enrollmentStorePath(flags)
	if store == "" || !filepath.IsAbs(store) {
		return errors.New("worker requires an absolute store path")
	}
	lock, err := service.AcquireProcessLock(filepath.Join(store, "enrollment", "worker.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	log, err := os.OpenFile(filepath.Join(store, "enrollment", "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	flags.externalWorker = true
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		if persistent {
			<-ctx.Done()
			return
		}
		ticker := time.NewTicker(enrollmentPolicyPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				requested, err := enroll.LoadControl(enroll.WorkerControlPath(store))
				if err == nil && (!requested.Present || !requested.Enabled) {
					cancel()
					return
				}
			}
		}
	}()
	fmt.Fprintf(log, "%s worker started pid=%d persistent=%t\n", time.Now().UTC().Format(time.RFC3339), os.Getpid(), persistent)
	runPeriodicEnrollment(ctx, flags, defaultServiceEnrollmentInterval, func(result FSEnrollmentApplyResult, err error) {
		fmt.Fprintf(log, "%s selected=%d applied=%d error=%v\n", time.Now().UTC().Format(time.RFC3339), result.Apply.Selected, result.Apply.Applied, err)
	})
	cancel()
	<-watchDone
	control := resolveEnrollmentControl(flags, defaultServiceEnrollmentInterval)
	phase := enroll.PhaseDisabled
	if persistent {
		phase = enroll.PhaseStopped
	}
	progress := applyEnrollmentControl(newEnrollmentProgress(flags, control, phase), flags, control)
	publishEnrollmentProgress(flags, progress)
	fmt.Fprintf(log, "%s worker stopped\n", time.Now().UTC().Format(time.RFC3339))
	return nil
}

func newFSEnrollStopCommand() *cobra.Command {
	var flags enrollmentFlags
	var apply bool
	command := &cobra.Command{Use: "stop", Short: "Stop only the separate enrollment worker", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !apply {
				return errors.New("worker stop requires --apply")
			}
			store := enrollmentStorePath(flags)
			if !filepath.IsAbs(store) {
				return errors.New("absolute store path required")
			}
			control, err := enroll.LoadControl(enroll.WorkerControlPath(store))
			if err != nil {
				return err
			}
			if !control.Present {
				return nil
			}
			control.Enabled = false
			if err := enroll.SaveControl(enroll.WorkerControlPath(store), control); err != nil {
				return err
			}
			deadline := time.Now().Add(30 * time.Second)
			for {
				status, err := service.InspectProcessLock(filepath.Join(store, "enrollment", "worker.lock"))
				if !status.Held {
					return err
				}
				if time.Now().After(deadline) {
					return errors.New("enrollment worker has not finished stopping")
				}
				select {
				case <-command.Context().Done():
					return command.Context().Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
		},
	}
	addEnrollmentFlags(command, &flags)
	command.Flags().BoolVar(&apply, "apply", false, "Disable and stop the separate worker")
	return command
}
