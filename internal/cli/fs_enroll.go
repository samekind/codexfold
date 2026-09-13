package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fsctl"
	"github.com/samekind/codexfold/internal/launcher"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
	"github.com/spf13/cobra"
)

const defaultServiceEnrollmentInterval = 5 * time.Minute

type enrollmentFlags struct {
	codexHome          string
	storeDir           string
	mountPoint         string
	nativeRoot         string
	stableFor          time.Duration
	batchSize          int
	archivedOnly       bool
	canonicalNamespace bool
	canary             bool
	jsonOutput         bool
	statusPaths        []string
}

type enrollmentApplyHooks struct {
	onPhase         func(string)
	onProgress      func(done, total int)
	stop            func() bool
	skipMaintenance bool
}

var errEnrollmentStopped = errors.New("automatic folding stopped")

var enrollmentPolicyPollInterval = 2 * time.Second

var enrollmentPolicyMinInterval = 30 * time.Second

type FSEnrollmentApplyResult struct {
	Plan        enroll.Plan                   `json:"plan"`
	Apply       enroll.ApplyResult            `json:"apply"`
	Maintenance FSEnrollmentMaintenanceResult `json:"maintenance"`
}

type FSEnrollmentMaintenanceResult struct {
	ReclaimDeferred    bool                            `json:"reclaim_deferred,omitempty"`
	NativeRetention    storage.NativeSnapshotRetention `json:"native_retention"`
	NativeCandidates   int                             `json:"native_candidates"`
	NativeRetired      int                             `json:"native_retired"`
	NativeDeferred     int                             `json:"native_deferred"`
	DeferredSessionIDs []string                        `json:"deferred_session_ids,omitempty"`
	LooseRetirementRan bool                            `json:"loose_retirement_ran"`
	StorageGC          storage.StorageGCResult         `json:"storage_gc"`
}

type enrollmentCycleReporter func(FSEnrollmentApplyResult, error)

var runEnrollmentCommand = func(ctx context.Context, args []string) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = enrollmentChildEnvironment(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func enrollmentChildEnvironment(environment []string) []string {
	prefix := launcher.ParentPIDEnvironment + "="
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		if strings.HasPrefix(value, prefix) {
			continue
		}
		result = append(result, value)
	}
	return result
}

var runServiceEnrollmentCycle = applyEnrollmentCycle

var discoverEnrollmentSessionStates = vfs.DiscoverSessionStates

var verifyEnrollmentPack = func(ctx context.Context, home, store string) error {
	return runEnrollmentCommand(ctx, []string{"pack", "doctor", "--codex-home", home, "--store", store})
}

var verifyEnrollmentFold = func(ctx context.Context, home, store string) error {
	return runEnrollmentCommand(ctx, []string{"doctor", "--codex-home", home, "--store", store})
}

var runEnrollmentStorageGC = func(ctx context.Context, store string) (storage.StorageGCResult, error) {
	return storage.Collect(ctx, storage.GCOptions{
		StoreDir: store, Apply: true,
		// Old packs are retained only by live leases or failed recovery proof,
		// not an unconditional second full copy of the compressed store.
		KeepPackGenerations: 1,
		AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
			return pack.AuthorizeGenerationRemoval(ctx, store, candidate)
		},
	})
}

var enrollmentStorageHealthProbe = requireEnrollmentStorageHealth

func newFSEnrollCommand() *cobra.Command {
	command := &cobra.Command{Use: "enroll", Short: "Plan and apply bounded automatic session enrollment"}
	command.AddCommand(newFSEnrollPlanCommand())
	command.AddCommand(newFSEnrollApplyCommand())
	return command
}

func newFSEnrollPlanCommand() *cobra.Command {
	var flags enrollmentFlags
	var record bool
	command := &cobra.Command{
		Use:   "plan",
		Short: "Report eligible and blocked sessions without changing routes",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			plan, store, err := buildEnrollmentPlan(command.Context(), flags)
			if err != nil {
				return err
			}
			if record {
				if err := enroll.SaveObservations(enrollmentObservationPath(store), plan.Observations); err != nil {
					return err
				}
			}
			if flags.jsonOutput {
				return writeJSON(command, plan)
			}
			if _, err = fmt.Fprintf(command.OutOrStdout(), "sessions=%d selected=%d observations=%d\n", len(plan.Decisions), len(plan.Selected), len(plan.Observations)); err != nil {
				return err
			}
			// Without this line the text report says only that nothing was
			// selected, and the reason is reachable only through --json.
			if summary := summarizeUnselectedReasons(plan.Decisions); summary != "" {
				_, err = fmt.Fprintf(command.OutOrStdout(), "not_selected: %s\n", summary)
			}
			return err
		},
	}
	addEnrollmentFlags(command, &flags)
	command.Flags().BoolVar(&record, "record-observations", false, "Persist this read-only stability observation for the next planning cycle")
	return command
}

func newFSEnrollApplyCommand() *cobra.Command {
	var flags enrollmentFlags
	var apply bool
	command := &cobra.Command{
		Use:   "apply",
		Short: "Apply the selected bounded enrollment batch",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !apply {
				return errors.New("enrollment apply requires --apply")
			}
			result, err := runEnrollmentCycle(command.Context(), flags)
			if err != nil {
				return err
			}
			if flags.jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "selected=%d applied=%d changed=%d managed=%d native_retention=%s native_retired=%d native_deferred=%d gc_removed=%d\n", result.Apply.Selected, result.Apply.Applied, result.Apply.SkippedChanged, result.Apply.SkippedManaged, result.Maintenance.NativeRetention, result.Maintenance.NativeRetired, result.Maintenance.NativeDeferred, result.Maintenance.StorageGC.RemovedCount)
			return err
		},
	}
	addEnrollmentFlags(command, &flags)
	command.Flags().BoolVar(&apply, "apply", false, "Run the bounded fold, pack, and canonical migration transactions")
	return command
}

func runEnrollmentCycle(ctx context.Context, flags enrollmentFlags) (FSEnrollmentApplyResult, error) {
	return applyEnrollmentCycle(ctx, flags, enrollmentApplyHooks{})
}

func (hooks enrollmentApplyHooks) stopIfRequested() error {
	if hooks.stop != nil && hooks.stop() {
		return errEnrollmentStopped
	}
	return nil
}

func (hooks enrollmentApplyHooks) reportPhase(phase string) {
	if hooks.onPhase != nil {
		hooks.onPhase(phase)
	}
}

func (hooks enrollmentApplyHooks) reportProgress(done, total int) {
	if hooks.onProgress != nil {
		hooks.onProgress(done, total)
	}
}

func applyEnrollmentCycle(ctx context.Context, flags enrollmentFlags, hooks enrollmentApplyHooks) (FSEnrollmentApplyResult, error) {
	home, err := codex.ResolveHome(flags.codexHome)
	if err != nil {
		return FSEnrollmentApplyResult{}, err
	}
	if err := requireFilesystemActivationAllowed(home); err != nil {
		return FSEnrollmentApplyResult{}, err
	}
	plan, store, err := buildEnrollmentPlan(ctx, flags)
	if err != nil {
		return FSEnrollmentApplyResult{}, err
	}
	if err := enroll.SaveObservations(enrollmentObservationPath(store), plan.Observations); err != nil {
		return FSEnrollmentApplyResult{}, err
	}
	mount := defaultMountPoint(home, flags.mountPoint)
	nativeRoot := flags.nativeRoot
	if nativeRoot == "" {
		nativeRoot = filepath.Join(home, "fold-native")
	}
	nativeRoot = filepath.Clean(nativeRoot)
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		return FSEnrollmentApplyResult{Plan: plan}, err
	}
	managed := make(map[string]struct{}, len(states))
	for _, state := range states {
		managed[state.SessionID] = struct{}{}
	}
	selected, applied, err := enroll.Revalidate(ctx, plan, enroll.RevalidateOptions{
		Limit: flags.batchSize,
		IsManaged: func(_ context.Context, sessionID string) (bool, error) {
			_, ok := managed[sessionID]
			return ok, nil
		},
	})
	result := FSEnrollmentApplyResult{Plan: plan, Apply: applied}
	if err != nil {
		return result, err
	}
	if len(selected) != 0 {
		if err := hooks.stopIfRequested(); err != nil {
			return result, err
		}
	}
	operationTotal := 0
	if len(selected) != 0 {
		// Each child command is a separately completed operation. The final
		// doctor remains outstanding until it succeeds, so progress cannot show
		// 100% while verification is still running.
		operationTotal = len(selected)*2 + 3
		if !hooks.skipMaintenance {
			operationTotal++ // Pack-only recovery verification and space reclamation.
		}
	}
	operationDone := 0
	hooks.reportProgress(operationDone, operationTotal)
	// Time the two halves of a cycle separately. Folding is what each extra
	// session costs; the pack rebuild is the toll the whole batch shares. The
	// next cycle sizes itself so the second stays small against the first.
	foldStarted := time.Now()
	var foldSeconds, packSeconds float64
	for _, decision := range selected {
		if err := hooks.stopIfRequested(); err != nil {
			return result, err
		}
		hooks.reportPhase(enroll.PhaseFolding)
		if err := revalidateEnrollmentDecision(ctx, home, decision, true); err != nil {
			return result, fmt.Errorf("prepare enrollment for %s: %w", decision.SessionID, err)
		}
		if err := runEnrollmentCommand(ctx, []string{"fold", decision.SessionID, "--codex-home", home, "--store", store, "--apply", "--overwrite"}); err != nil {
			return result, fmt.Errorf("fold enrollment for %s: %w", decision.SessionID, err)
		}
		operationDone++
		hooks.reportProgress(operationDone, operationTotal)
	}
	if len(selected) != 0 {
		foldSeconds = time.Since(foldStarted).Seconds()
		if err := hooks.stopIfRequested(); err != nil {
			return result, err
		}
		hooks.reportPhase(enroll.PhasePacking)
		packStarted := time.Now()
		if err := runEnrollmentCommand(ctx, []string{"pack", "build", "--codex-home", home, "--store", store}); err != nil {
			return result, fmt.Errorf("build enrollment pack for %d sessions: %w", len(selected), err)
		}
		operationDone++
		hooks.reportProgress(operationDone, operationTotal)
		if err := verifyEnrollmentPack(ctx, home, store); err != nil {
			return result, fmt.Errorf("verify enrollment pack for %d sessions: %w", len(selected), err)
		}
		packSeconds = time.Since(packStarted).Seconds()
		// Record before migration: what was measured is already true, and a
		// migration that fails should still teach the next cycle its own cost.
		// A tuning record that cannot be written costs the next cycle a good batch
		// size, not correctness, so it does not fail the cycle.
		_ = enroll.SaveTuning(
			enrollmentTuningPath(store),
			enroll.LoadTuning(enrollmentTuningPath(store)).Observe(packSeconds, foldSeconds, len(selected)),
		)
		operationDone++
		hooks.reportProgress(operationDone, operationTotal)
	}
	// A session that cannot be routed is this session's problem, not the batch's.
	// Returning here skips reclamation, and reclamation is the half of the cycle
	// that shrinks the store: the pack build above has already written a new
	// generation, so aborting leaves the store larger than it started and does it
	// again every cycle for as long as one session stays stuck. Collect the
	// failures, keep going, and report them after the space has been reclaimed.
	var migrationErrors []error
	for _, decision := range selected {
		if err := hooks.stopIfRequested(); err != nil {
			return result, err
		}
		hooks.reportPhase(enroll.PhaseMigrating)
		if err := revalidateEnrollmentDecision(ctx, home, decision, false); err != nil {
			migrationErrors = append(migrationErrors, fmt.Errorf("commit enrollment for %s: %w", decision.SessionID, err))
			continue
		}
		arguments := []string{
			"fs", "migrate", decision.SessionID, "--codex-home", home, "--store", store,
			"--mount", mount, "--canonical-namespace", "--native-root", nativeRoot, "--apply",
		}
		if flags.canary {
			arguments = append(arguments, "--compatibility-canary", "--cli", "none", "--desktop-app", "none")
		}
		if err := runEnrollmentCommand(ctx, arguments); err != nil {
			if ctx.Err() != nil {
				return result, err
			}
			migrationErrors = append(migrationErrors, fmt.Errorf("migrate enrollment for %s: %w", decision.SessionID, err))
			continue
		}
		result.Apply.Applied++
		operationDone++
		hooks.reportProgress(operationDone, operationTotal)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err := hooks.stopIfRequested(); err != nil {
		return result, err
	}
	if len(selected) != 0 {
		if err := verifyEnrollmentFold(ctx, home, store); err != nil {
			return result, fmt.Errorf("verify enrollment after migration: %w", err)
		}
		operationDone++
		hooks.reportProgress(operationDone, operationTotal)
	}
	if hooks.skipMaintenance {
		return result, errors.Join(migrationErrors...)
	}
	hooks.reportPhase(enroll.PhaseReclaiming)
	maintenance, maintenanceErr := runEnrollmentMaintenance(ctx, home, store, result.Apply.Applied, len(migrationErrors) == 0)
	result.Maintenance = maintenance
	if maintenanceErr == nil && !maintenance.ReclaimDeferred && operationTotal > 0 {
		operationDone++
		hooks.reportProgress(operationDone, operationTotal)
	}
	return result, errors.Join(append(migrationErrors, maintenanceErr)...)
}

func revalidateEnrollmentDecision(ctx context.Context, home string, decision enroll.Decision, validateRollout bool) error {
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return err
	}
	current, err := findSession(sessions, decision.SessionID)
	if err != nil {
		return err
	}
	if filepath.Clean(current.RolloutPath) != filepath.Clean(decision.RolloutPath) {
		return errors.New("session route changed after enrollment planning")
	}
	writers, err := enrollmentWriterProbe(ctx, []codex.Session{current})
	if err != nil {
		return fmt.Errorf("recheck native session writer: %w", err)
	}
	if writers[current.ID] {
		return errors.New("session acquired an active native writer after enrollment planning")
	}
	if validateRollout {
		if _, err := mountfs.ValidateNativeRollout(ctx, decision.RolloutPath); err != nil {
			return fmt.Errorf("native rollout is not eligible for transparent routing: %w", err)
		}
	}
	return nil
}

func runPeriodicEnrollment(ctx context.Context, flags enrollmentFlags, interval time.Duration, report enrollmentCycleReporter) {
	control := resolveEnrollmentControl(flags, interval)
	nextApply := time.Time{}
	if control.Enabled {
		nextApply = time.Now()
	}
	progress := applyEnrollmentControl(newEnrollmentProgress(flags, control, enroll.PhaseDisabled), flags, control)
	if control.Enabled {
		progress.Phase = enroll.PhaseIdle
		progress.NextCheckAt = nextApply
	}
	publishEnrollmentProgress(flags, progress)

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		wait := enrollmentPolicyPollInterval
		if control.Enabled && !nextApply.IsZero() {
			if remaining := time.Until(nextApply); remaining < wait {
				wait = remaining
			}
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		previous := control
		control = resolveEnrollmentControl(flags, interval)
		now := time.Now()
		if enrollmentControlChanged(previous, control) {
			if control.Enabled && !previous.Enabled {
				nextApply = now
			} else if control.Enabled && previous.Interval != control.Interval {
				nextApply = now.Add(effectiveEnrollmentInterval(control, interval))
			}
			if !control.Enabled {
				nextApply = time.Time{}
			}
			progress = applyEnrollmentControl(progress, flags, control)
			if control.Enabled {
				progress.Phase = enroll.PhaseIdle
				progress.NextCheckAt = nextApply
			} else {
				progress.Phase = enroll.PhaseDisabled
				progress.NextCheckAt = time.Time{}
			}
			publishEnrollmentProgress(flags, progress)
			continue
		}
		if !control.Enabled || nextApply.IsZero() || now.Before(nextApply) {
			progress = applyEnrollmentControl(progress, flags, control)
			if control.Enabled {
				progress.Phase = enroll.PhaseIdle
				progress.NextCheckAt = nextApply
			} else {
				progress.Phase = enroll.PhaseDisabled
				progress.NextCheckAt = time.Time{}
			}
			publishEnrollmentProgress(flags, progress)
			continue
		}

		cycleFlags := flagsWithEnrollmentControl(flags, control)
		progress = applyEnrollmentControl(progress, flags, control)
		progress.Phase = enroll.PhaseChecking
		progress.CycleTotal = 0
		progress.CycleDone = 0
		progress.NextCheckAt = time.Time{}
		progress.LastError = ""
		publishEnrollmentProgress(flags, progress)

		cycleCtx, cycleCancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			poll := enrollmentPolicyPollInterval
			if poll <= 0 {
				poll = 100 * time.Millisecond
			}
			ticker := time.NewTicker(poll)
			defer ticker.Stop()
			for {
				select {
				case <-cycleCtx.Done():
					return
				case <-ticker.C:
					if next := resolveEnrollmentControl(flags, interval); !next.Enabled {
						cycleCancel()
						return
					}
				}
			}
		}()
		result, err := runServiceEnrollmentCycle(cycleCtx, cycleFlags, enrollmentApplyHooks{
			onPhase: func(phase string) {
				progress.Phase = phase
				publishEnrollmentProgress(flags, progress)
			},
			onProgress: func(done, total int) {
				progress.CycleDone = done
				progress.CycleTotal = total
				publishEnrollmentProgress(flags, progress)
			},
			stop: func() bool {
				return !resolveEnrollmentControl(flags, interval).Enabled
			},
		})
		cycleCancel()
		<-watchDone
		if report != nil {
			report(result, err)
		}
		if ctx.Err() != nil {
			return
		}
		progress = applyEnrollmentControl(progress, flags, control)
		progress.WaitingCount = enrollmentRemainingWaiting(result)
		progress.WaitingKnown = true
		progress.ManagedCount = enrollmentManagedCount(cycleFlags, result)
		if progress.CycleTotal > 0 && err == nil && !result.Maintenance.ReclaimDeferred {
			progress.CycleDone = progress.CycleTotal
		}
		if err != nil && !errors.Is(err, errEnrollmentStopped) && !errors.Is(err, context.Canceled) {
			progress.LastError = "retry"
			progress.ErrorKind = "operation"
		} else {
			progress.LastError = ""
			progress.ErrorKind = ""
		}
		control = resolveEnrollmentControl(flags, interval)
		if control.Enabled {
			nextApply = time.Now().Add(effectiveEnrollmentInterval(control, interval))
			progress.Phase = enroll.PhaseIdle
			if err == nil && result.Maintenance.ReclaimDeferred {
				progress.Phase = enroll.PhaseWaitingReclaim
			}
			progress.NextCheckAt = nextApply
		} else {
			nextApply = time.Time{}
			progress.Phase = enroll.PhaseDisabled
			progress.NextCheckAt = time.Time{}
		}
		progress = applyEnrollmentControl(progress, flags, control)
		publishEnrollmentProgress(flags, progress)
	}
}

// The plan describes the start of the cycle; successfully migrated sessions
// must not remain in the displayed queue until the next scheduled scan.
func enrollmentRemainingWaiting(result FSEnrollmentApplyResult) int {
	return max(0, enroll.WaitingCount(result.Plan)-result.Apply.Applied)
}

func resolveEnrollmentControl(flags enrollmentFlags, flagInterval time.Duration) enroll.Control {
	path := enrollmentControlPath(flags)
	if path == "" {
		return flagEnrollmentControl(flags, flagInterval)
	}
	control, err := enroll.LoadControl(path)
	if err != nil {
		return enroll.Control{
			Present:     true,
			ConfigError: fmt.Sprintf("enrollment policy is invalid: %v", err),
		}
	}
	if !control.Present {
		return flagEnrollmentControl(flags, flagInterval)
	}
	if control.Enabled && control.Interval > 0 && control.Interval < enrollmentPolicyMinInterval {
		control.Interval = enrollmentPolicyMinInterval
	}
	return control
}

func flagEnrollmentControl(flags enrollmentFlags, flagInterval time.Duration) enroll.Control {
	return enroll.Control{
		Enabled:      flagInterval > 0,
		Interval:     flagInterval,
		StableFor:    flags.stableFor,
		ArchivedOnly: flags.archivedOnly,
		BatchSize:    flags.batchSize,
	}
}

func enrollmentControlPath(flags enrollmentFlags) string {
	if flags.storeDir != "" {
		if !filepath.IsAbs(flags.storeDir) {
			return ""
		}
		return enroll.ControlPath(flags.storeDir)
	}
	if flags.codexHome == "" {
		return ""
	}
	home, err := codex.ResolveHome(flags.codexHome)
	if err != nil {
		return ""
	}
	return enroll.ControlPath(resolveFoldStore(home, flags.storeDir))
}

func effectiveEnrollmentInterval(control enroll.Control, flagInterval time.Duration) time.Duration {
	if control.Present {
		if control.Interval < enrollmentPolicyMinInterval {
			return enrollmentPolicyMinInterval
		}
		return control.Interval
	}
	if control.Interval > 0 {
		return control.Interval
	}
	return flagInterval
}

func enrollmentControlChanged(previous enroll.Control, next enroll.Control) bool {
	return previous.Present != next.Present ||
		previous.ConfigError != next.ConfigError ||
		previous.Enabled != next.Enabled ||
		previous.Interval != next.Interval ||
		previous.StableFor != next.StableFor ||
		previous.ArchivedOnly != next.ArchivedOnly ||
		previous.BatchSize != next.BatchSize
}

func flagsWithEnrollmentControl(flags enrollmentFlags, control enroll.Control) enrollmentFlags {
	if control.StableFor > 0 {
		flags.stableFor = control.StableFor
	}
	// Zero is the automatic setting and has to reach the planner as zero; only a
	// number the user actually chose overrides it.
	if control.Present {
		flags.batchSize = control.BatchSize
	}
	flags.archivedOnly = control.ArchivedOnly
	return flags
}

func newEnrollmentProgress(flags enrollmentFlags, control enroll.Control, phase string) enroll.Progress {
	progress := enroll.Progress{
		Enabled:      control.Enabled,
		Interval:     control.Interval,
		StableFor:    control.StableFor,
		ArchivedOnly: control.ArchivedOnly,
		Phase:        phase,
		ManagedCount: enrollmentManagedCount(flags, FSEnrollmentApplyResult{}),
	}
	if store := enrollmentStorePath(flags); store != "" {
		progress.StorePath = store
	}
	return progress
}

func applyEnrollmentControl(progress enroll.Progress, flags enrollmentFlags, control enroll.Control) enroll.Progress {
	progress.Enabled = control.Enabled
	progress.Interval = control.Interval
	progress.StableFor = control.StableFor
	progress.ArchivedOnly = control.ArchivedOnly
	if control.ConfigError != "" {
		progress.LastError = control.ConfigError
		progress.ErrorKind = "configuration"
	} else if progress.ErrorKind == "configuration" {
		progress.LastError = ""
		progress.ErrorKind = ""
	}
	if store := enrollmentStorePath(flags); store != "" {
		progress.StorePath = store
	}
	progress.ManagedCount = enrollmentManagedCount(flags, FSEnrollmentApplyResult{})
	return progress
}

func enrollmentStorePath(flags enrollmentFlags) string {
	if flags.storeDir != "" {
		return filepath.Clean(flags.storeDir)
	}
	if flags.codexHome == "" {
		return ""
	}
	home, err := codex.ResolveHome(flags.codexHome)
	if err != nil {
		return ""
	}
	return resolveFoldStore(home, flags.storeDir)
}

func enrollmentManagedCount(flags enrollmentFlags, result FSEnrollmentApplyResult) int {
	if result.Apply.Applied > 0 || len(result.Plan.Decisions) > 0 {
		count := 0
		for _, decision := range result.Plan.Decisions {
			for _, reason := range decision.Reasons {
				if reason == enroll.ReasonAlreadyManaged {
					count++
					break
				}
			}
		}
		if result.Apply.Applied > 0 {
			return count + result.Apply.Applied
		}
		if count > 0 {
			return count
		}
	}
	store := enrollmentStorePath(flags)
	if store == "" {
		return 0
	}
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		return 0
	}
	return len(states)
}

func publishEnrollmentProgress(flags enrollmentFlags, progress enroll.Progress) {
	paths := append([]string(nil), flags.statusPaths...)
	if store := enrollmentStorePath(flags); store != "" {
		paths = append(paths, enroll.ProgressPath(store))
	}
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		_ = enroll.SaveProgress(path, progress)
	}
}

// summarizeUnselectedReasons counts why each unselected session was held back,
// most frequent first, so the text report explains a zero selection without
// requiring --json. A session with no recorded reason is counted as "unknown"
// rather than silently dropped.
func summarizeUnselectedReasons(decisions []enroll.Decision) string {
	counts := make(map[string]int)
	for _, decision := range decisions {
		if decision.Selected {
			continue
		}
		if len(decision.Reasons) == 0 {
			counts["unknown"]++
			continue
		}
		for _, reason := range decision.Reasons {
			counts[string(reason)]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return strings.Join(parts, " ")
}

func addEnrollmentFlags(command *cobra.Command, flags *enrollmentFlags) {
	command.Flags().StringVar(&flags.codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&flags.storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&flags.mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().BoolVar(&flags.canonicalNamespace, "canonical-namespace", false, "Plan canonical-path enrollment without changing SQLite routes")
	command.Flags().StringVar(&flags.nativeRoot, "native-root", "", "Canonical native backing root; defaults to <codex-home>/fold-native")
	command.Flags().DurationVar(&flags.stableFor, "stable-for", time.Hour, "Required unchanged observation window")
	command.Flags().IntVar(&flags.batchSize, "batch-size", 1, "Maximum sessions selected per cycle")
	command.Flags().BoolVar(&flags.canary, "enrollment-canary", false, "Enable additional isolated-home constraints for a validation canary")
	command.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON output")
}

func buildEnrollmentPlan(ctx context.Context, flags enrollmentFlags) (enroll.Plan, string, error) {
	home, err := codex.ResolveHome(flags.codexHome)
	if err != nil {
		return enroll.Plan{}, "", err
	}
	store := resolveFoldStore(home, flags.storeDir)
	mount := defaultMountPoint(home, flags.mountPoint)
	nativeRoot := flags.nativeRoot
	if nativeRoot == "" {
		nativeRoot = filepath.Join(home, "fold-native")
	}
	nativeRoot = filepath.Clean(nativeRoot)
	if flags.canary {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return enroll.Plan{}, "", err
		}
		if err := validateCompatibilityCanary(home, filepath.Join(userHome, ".codex"), store, flags.canonicalNamespace, compatibilityFlags{cliPath: "none", desktopPath: "none"}); err != nil {
			return enroll.Plan{}, "", err
		}
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return enroll.Plan{}, "", err
	}
	writers, err := enrollmentWriterProbe(ctx, sessions)
	if err != nil {
		return enroll.Plan{}, "", fmt.Errorf("probe native session writers: %w", err)
	}
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		return enroll.Plan{}, "", err
	}
	managed := make(map[string]struct{}, len(states))
	for _, state := range states {
		managed[state.SessionID] = struct{}{}
	}
	observations, err := enroll.LoadObservations(enrollmentObservationPath(store))
	if err != nil {
		return enroll.Plan{}, "", err
	}
	mountHealthy := mountHealthProbe(mount) == nil
	readiness := canonicalNamespaceReadiness{}
	if flags.canonicalNamespace && mountHealthy {
		readiness = enrollmentCanonicalNamespaceReadinessProbe(home, mount, nativeRoot)
	}
	guard, err := storage.DefaultGuard(store)
	if err != nil {
		return enroll.Plan{}, "", err
	}
	now := time.Now()
	// A batch the user did not choose is sized from what the last cycle measured,
	// so the pack rebuild it has to amortize stays a small share of the work.
	batchSize := flags.batchSize
	if batchSize <= 0 {
		batchSize = enroll.AutoBatchSize(enroll.LoadTuning(enrollmentTuningPath(store)), enroll.DefaultAutoBatchLimits)
	}
	input := enroll.Input{
		Sessions: sessions, Managed: managed, Previous: observations, Now: now,
		Policy: enroll.Policy{StableFor: flags.stableFor, BatchSize: batchSize, ArchivedOnly: flags.archivedOnly},
		Gates: enroll.Gates{
			DoctorHealthy: true, MountHealthy: mountHealthy,
			CanonicalNamespace: flags.canonicalNamespace, NamespaceActive: readiness.Active, NamespaceReady: readiness.Ready,
			EnrollmentAllowed: flags.canary || automaticEnrollmentAllowed(verifiedCapability()),
		},
		WriterActive: func(_ context.Context, session codex.Session) (bool, error) {
			return writers[session.ID], nil
		},
		Budget: guard,
	}
	plan, err := enroll.Build(ctx, input)
	if err != nil || len(plan.Selected) == 0 {
		return plan, store, err
	}
	// Full fold and pack verification reconstructs every managed session. Keep
	// it off the no-op polling path, but require it before any batch mutation.
	if enrollmentStorageHealthProbe(ctx, store) == nil {
		return plan, store, nil
	}
	// A pack build that was refused — most often because the disk was briefly
	// too full — leaves the objects it should have packed still referenced by
	// manifests the pack cannot read. The doctor reports exactly that, and
	// reporting it is all the cycle used to do: the gate then blocked every
	// candidate, so the store stayed unhealthy and nothing folded again, on
	// every cycle, until a person noticed. Packing what is already on disk is
	// the same step the batch would have run anyway and it removes nothing, so
	// attempt it once before giving the cycle up. If it is still refused, the
	// gate closes exactly as before.
	if healErr := runEnrollmentCommand(ctx, []string{"pack", "build", "--codex-home", home, "--store", store}); healErr == nil {
		if enrollmentStorageHealthProbe(ctx, store) == nil {
			return plan, store, nil
		}
	}
	input.Gates.DoctorHealthy = false
	plan, err = enroll.Build(ctx, input)
	return plan, store, err
}

func requireEnrollmentStorageHealth(ctx context.Context, store string) error {
	foldReport, err := doctorFoldStore(ctx, store)
	if err != nil {
		return err
	}
	if foldReport.IssueCount != 0 {
		return fmt.Errorf("fold doctor reported %d issues", foldReport.IssueCount)
	}
	packReport, err := pack.Doctor(ctx, store)
	if err != nil {
		return err
	}
	if packReport.IssueCount == 0 {
		return nil
	}
	if _, currentErr := os.Lstat(filepath.Join(filepath.Clean(store), "packs", "CURRENT")); errors.Is(currentErr, os.ErrNotExist) {
		states, stateErr := vfs.DiscoverSessionStates(store)
		if stateErr != nil {
			return stateErr
		}
		if len(states) == 0 {
			return nil
		}
	}
	return fmt.Errorf("pack doctor reported %d issues", packReport.IssueCount)
}

func automaticEnrollmentAllowed(capability fsctl.Capability) bool {
	return capability != fsctl.StorageEngine
}

func enrollmentTuningPath(store string) string {
	return filepath.Join(filepath.Clean(store), "enrollment", "tuning.json")
}

func enrollmentObservationPath(store string) string {
	return filepath.Join(filepath.Clean(store), "enrollment", "observations.json")
}

// runEnrollmentMaintenance reclaims space after a cycle. retireNative is false
// when some session in the batch could not be routed: reclaiming data the pack
// already holds is always safe, but deleting a user's original file is not
// something to start doing while part of the cycle is failing. Those sessions
// defer to the next clean cycle.
func runEnrollmentMaintenance(ctx context.Context, home string, store string, newlyApplied int, retireNative bool) (FSEnrollmentMaintenanceResult, error) {
	retention, err := storage.LoadRetentionPolicy(store)
	if err != nil {
		return FSEnrollmentMaintenanceResult{}, fmt.Errorf("load storage retention policy: %w", err)
	}
	result := FSEnrollmentMaintenanceResult{NativeRetention: retention.NativeSnapshots}
	states, err := discoverEnrollmentSessionStates(store)
	if err != nil {
		return result, err
	}
	var maintenanceErrors []error
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(maintenanceErrors, err)...)
		}
		if state.NativeSnapshot.Path == "" {
			continue
		}
		result.NativeCandidates++
		if !retireNative || retention.NativeSnapshots == storage.NativeSnapshotRetentionManual {
			result.NativeDeferred++
			result.DeferredSessionIDs = append(result.DeferredSessionIDs, state.SessionID)
			continue
		}
		err := runEnrollmentCommand(ctx, []string{"fs", "retire-native", state.SessionID, "--codex-home", home, "--store", store, "--apply"})
		if err == nil {
			result.NativeRetired++
			continue
		}
		result.NativeDeferred++
		result.DeferredSessionIDs = append(result.DeferredSessionIDs, state.SessionID)
		if strings.Contains(err.Error(), "active writer") {
			result.ReclaimDeferred = true
		} else {
			maintenanceErrors = append(maintenanceErrors, fmt.Errorf("retire native snapshot for %s: %w", state.SessionID, err))
		}
	}
	loosePresent, looseErr := enrollmentHasLooseObjects(store)
	if looseErr != nil {
		maintenanceErrors = append(maintenanceErrors, looseErr)
	}
	if newlyApplied > 0 || result.NativeCandidates > 0 || len(states) > 0 && loosePresent {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(maintenanceErrors, err)...)
		}
		if err := runEnrollmentCommand(ctx, []string{"pack", "retire-loose", "--codex-home", home, "--store", store, "--apply"}); err != nil {
			if strings.Contains(err.Error(), storage.ErrManagedSessionBusy.Error()) {
				result.ReclaimDeferred = true
			} else {
				maintenanceErrors = append(maintenanceErrors, fmt.Errorf("retire loose objects: %w", err))
			}
		} else {
			result.LooseRetirementRan = true
		}
	}
	// Retry collection on later enabled cycles after a reader has released an
	// old generation, even when every native snapshot was already retired.
	if result.LooseRetirementRan || result.NativeRetired > 0 || len(states) > 0 {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(maintenanceErrors, err)...)
		}
		gc, err := runEnrollmentStorageGC(ctx, store)
		result.StorageGC = gc
		if errors.Is(err, storage.ErrManagedSessionBusy) {
			result.ReclaimDeferred = true
		} else if err != nil {
			maintenanceErrors = append(maintenanceErrors, fmt.Errorf("collect old storage generations: %w", err))
		}
	}
	return result, errors.Join(maintenanceErrors...)
}

// A previous cycle may have retired the native snapshot but deferred loose
// cleanup behind a live writer. Retry that work without requiring a new fold.
func enrollmentHasLooseObjects(store string) (bool, error) {
	found := false
	err := filepath.WalkDir(filepath.Join(store, "objects"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".zst") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return found, err
}
