package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fsctl"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
	"github.com/spf13/cobra"
)

type enrollmentFlags struct {
	codexHome          string
	storeDir           string
	mountPoint         string
	nativeRoot         string
	stableFor          time.Duration
	batchSize          int
	canonicalNamespace bool
	canary             bool
	jsonOutput         bool
}

type FSEnrollmentApplyResult struct {
	Plan  enroll.Plan        `json:"plan"`
	Apply enroll.ApplyResult `json:"apply"`
}

type enrollmentCycleReporter func(FSEnrollmentApplyResult, error)

var runEnrollmentCommand = func(ctx context.Context, args []string) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

var runServiceEnrollmentCycle = runEnrollmentCycle

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
			_, err = fmt.Fprintf(command.OutOrStdout(), "sessions=%d selected=%d observations=%d\n", len(plan.Decisions), len(plan.Selected), len(plan.Observations))
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
			_, err = fmt.Fprintf(command.OutOrStdout(), "selected=%d applied=%d changed=%d managed=%d\n", result.Apply.Selected, result.Apply.Applied, result.Apply.SkippedChanged, result.Apply.SkippedManaged)
			return err
		},
	}
	addEnrollmentFlags(command, &flags)
	command.Flags().BoolVar(&apply, "apply", false, "Run the bounded fold, pack, and canonical migration transactions")
	return command
}

func runEnrollmentCycle(ctx context.Context, flags enrollmentFlags) (FSEnrollmentApplyResult, error) {
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
	applied, err := enroll.Apply(ctx, plan, enroll.ApplyOptions{
		Limit: flags.batchSize,
		IsManaged: func(_ context.Context, sessionID string) (bool, error) {
			states, err := vfs.DiscoverSessionStates(store)
			if err != nil {
				return false, err
			}
			for _, state := range states {
				if state.SessionID == sessionID {
					return true, nil
				}
			}
			return false, nil
		},
		Apply: func(ctx context.Context, decision enroll.Decision) error {
			if _, err := mountfs.ValidateNativeRollout(ctx, decision.RolloutPath); err != nil {
				return fmt.Errorf("native rollout is not eligible for transparent routing: %w", err)
			}
			return applyEnrollmentCommands(ctx, home, store, mount, nativeRoot, decision.SessionID, flags.canary)
		},
	})
	result := FSEnrollmentApplyResult{Plan: plan, Apply: applied}
	return result, err
}

func runPeriodicEnrollment(ctx context.Context, flags enrollmentFlags, interval time.Duration, report enrollmentCycleReporter) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := runServiceEnrollmentCycle(ctx, flags)
			if report != nil {
				report(result, err)
			}
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func addEnrollmentFlags(command *cobra.Command, flags *enrollmentFlags) {
	command.Flags().StringVar(&flags.codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&flags.storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&flags.mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().BoolVar(&flags.canonicalNamespace, "canonical-namespace", false, "Plan canonical-path enrollment without changing SQLite routes")
	command.Flags().StringVar(&flags.nativeRoot, "native-root", "", "Canonical native backing root; defaults to <codex-home>/fold-native")
	command.Flags().DurationVar(&flags.stableFor, "stable-for", time.Hour, "Required unchanged observation window")
	command.Flags().IntVar(&flags.batchSize, "batch-size", 1, "Maximum sessions selected per cycle")
	command.Flags().BoolVar(&flags.canary, "enrollment-canary", false, "Allow explicit enrollment only in an isolated Codex home while capability remains preview")
	command.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON output")
}

func buildEnrollmentPlan(ctx context.Context, flags enrollmentFlags) (enroll.Plan, string, error) {
	home, err := codex.ResolveHome(flags.codexHome)
	if err != nil {
		return enroll.Plan{}, "", err
	}
	store := resolveFoldStore(home, flags.storeDir)
	mount := defaultMountPoint(home, flags.mountPoint)
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
	doctorHealthy := requireEnrollmentStorageHealth(ctx, store) == nil
	compatibilityApproved := flags.canary
	if !flags.canary {
		compatibility, err := evaluateCompatibility(ctx, store, defaultCompatibilityFlags())
		if err == nil {
			compatibilityApproved = len(compatibility.DetectionErrors) == 0 && compatibility.Evaluation.Approved
		}
	}
	mountHealthy := mountHealthProbe(mount) == nil
	guard, err := storage.DefaultGuard(store)
	if err != nil {
		return enroll.Plan{}, "", err
	}
	plan, err := enroll.Build(ctx, enroll.Input{
		Sessions: sessions, Managed: managed, Previous: observations, Now: time.Now(),
		Policy: enroll.Policy{StableFor: flags.stableFor, BatchSize: flags.batchSize, ArchivedOnly: true},
		Gates: enroll.Gates{
			DoctorHealthy: doctorHealthy, CompatibilityApproved: compatibilityApproved, MountHealthy: mountHealthy,
			CanonicalNamespace: flags.canonicalNamespace, EnrollmentAllowed: flags.canary || automaticEnrollmentAllowed(verifiedCapability()),
		},
		WriterActive: func(_ context.Context, session codex.Session) (bool, error) {
			return writers[session.ID], nil
		},
		Budget: guard,
	})
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
	return capability == fsctl.CrossPlatformReady || strings.HasPrefix(string(capability), "production-ready:")
}

func enrollmentObservationPath(store string) string {
	return filepath.Join(filepath.Clean(store), "enrollment", "observations.json")
}

func applyEnrollmentCommands(ctx context.Context, home string, store string, mount string, nativeRoot string, sessionID string, canary bool) error {
	commands := [][]string{
		{"fold", sessionID, "--codex-home", home, "--store", store, "--apply", "--overwrite"},
		{"pack", "build", "--codex-home", home, "--store", store},
		{"fs", "migrate", sessionID, "--codex-home", home, "--store", store, "--mount", mount, "--canonical-namespace", "--native-root", nativeRoot, "--apply"},
	}
	if canary {
		commands[2] = append(commands[2], "--compatibility-canary", "--cli", "none", "--desktop-app", "none")
	}
	for _, command := range commands {
		if err := runEnrollmentCommand(ctx, command); err != nil {
			return err
		}
	}
	return nil
}
