package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jstar0/codexfold/internal/cdc"
	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/compat"
	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/fsctl"
	"github.com/jstar0/codexfold/internal/mountfs"
	"github.com/jstar0/codexfold/internal/pack"
	"github.com/jstar0/codexfold/internal/vfs"
	"github.com/spf13/cobra"
)

type FSMigrateResult struct {
	SessionID string             `json:"session_id"`
	Native    vfs.NativeFile     `json:"native"`
	Target    string             `json:"target"`
	Shadow    fsctl.ShadowResult `json:"shadow"`
	DryRun    bool               `json:"dry_run"`
	Routed    bool               `json:"routed"`
}

type FSCompatibilityResult struct {
	Installed       []compat.ClientVersion `json:"installed,omitempty"`
	Contracts       int                    `json:"contracts"`
	DetectionErrors []string               `json:"detection_errors,omitempty"`
	Evaluation      compat.Evaluation      `json:"evaluation"`
}

type FSServeResult struct {
	MountPoint      string `json:"mount_point"`
	ManagedSessions int    `json:"managed_sessions"`
	DryRun          bool   `json:"dry_run"`
}

type FSRollbackResult struct {
	SessionID string         `json:"session_id"`
	From      string         `json:"from"`
	Target    vfs.NativeFile `json:"target"`
	DryRun    bool           `json:"dry_run"`
	Routed    bool           `json:"routed"`
}

type FSCompactResult struct {
	SessionID         string `json:"session_id"`
	CurrentGeneration uint64 `json:"current_generation"`
	NextGeneration    uint64 `json:"next_generation"`
	Bytes             int64  `json:"bytes,omitempty"`
	SHA256            string `json:"sha256,omitempty"`
	DryRun            bool   `json:"dry_run"`
}

type FSRecoverResult struct {
	SessionIDs []string `json:"session_ids"`
	Recovered  int      `json:"recovered"`
	DryRun     bool     `json:"dry_run"`
}

type compatibilityFlags struct {
	contractsPath string
	cliPath       string
	desktopPath   string
}

func newFSCommand() *cobra.Command {
	command := &cobra.Command{Use: "fs", Short: "Operate the transparent session filesystem"}
	command.AddCommand(newFSStatusCommand())
	command.AddCommand(newFSDoctorCommand())
	command.AddCommand(newFSCompatibilityCommand())
	command.AddCommand(newFSBenchmarkCommand())
	command.AddCommand(newFSServeCommand())
	command.AddCommand(newFSMigrateCommand())
	command.AddCommand(newFSRollbackCommand())
	command.AddCommand(newFSCompactCommand())
	command.AddCommand(newFSRecoverCommand())
	command.AddCommand(newFSServiceCommand())
	return command
}

func newFSStatusCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Report the highest verified filesystem capability",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			status, err := fsctl.NewStatus(fsctl.StorageEngine, runtime.GOOS)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, status)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "capability=%s platform=%s\n", status.Capability, status.Platform)
			return err
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSDoctorCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var mountPoint string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Verify filesystem storage, state, route, client, daemon, and mount components",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			mount := defaultMountPoint(home, mountPoint)
			report := fsDoctor(command.Context(), home, store, mount)
			if jsonOutput {
				return writeJSON(command, report)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "healthy=%t issues=%d daemon=%t mount=%t pack=%t manifest=%t\n", report.Healthy, report.IssueCount, report.ComponentHealth[fsctl.ComponentDaemon], report.ComponentHealth[fsctl.ComponentMount], report.ComponentHealth[fsctl.ComponentPack], report.ComponentHealth[fsctl.ComponentManifest])
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSCompatibilityCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var flags compatibilityFlags
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "compatibility",
		Short: "Evaluate installed Codex clients against exact-version contracts",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			result, err := evaluateCompatibility(command.Context(), resolveFoldStore(home, storeDir), flags)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "approved=%t quarantine=%t installed=%d contracts=%d detection_errors=%d\n", result.Evaluation.Approved, result.Evaluation.Quarantine, len(result.Installed), result.Contracts, len(result.DetectionErrors))
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	addCompatibilityFlags(command, &flags)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSBenchmarkCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var jsonOutput bool
	var options fsctl.BenchmarkOptions
	command := &cobra.Command{
		Use:   "benchmark <session-id>",
		Short: "Compare native and packed virtual reads without changing routes",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			session, manifest, resolver, view, err := openFoldView(home, store, args[0])
			if err != nil {
				return err
			}
			defer resolver.Close()
			if manifest.Source.SHA256 == "" {
				return errors.New("manifest source digest is missing")
			}
			report, err := fsctl.Benchmark(command.Context(), session.RolloutPath, view, options)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, report)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "native=%.0fB/s virtual=%.0fB/s random_p95=%s go_sys=%s\n", report.Native.BytesPerSecond, report.Virtual.BytesPerSecond, report.Random.P95, formatBytes(int64(report.GoSysBytes)))
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().IntVar(&options.SequentialBlockBytes, "sequential-block-bytes", 0, "Sequential read block size")
	command.Flags().IntVar(&options.RandomBlockBytes, "random-block-bytes", 0, "Random read block size")
	command.Flags().IntVar(&options.RandomReads, "random-reads", 0, "Random read count")
	command.Flags().Int64Var(&options.Seed, "seed", 1, "Deterministic random seed")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSServeCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var mountPoint string
	var apply bool
	var foreground bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "serve",
		Short: "Mount managed sessions and hot-load newly enrolled state",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			mount := defaultMountPoint(home, mountPoint)
			states, err := vfs.DiscoverSessionStates(store)
			if err != nil {
				return err
			}
			result := FSServeResult{MountPoint: mount, ManagedSessions: len(states), DryRun: !apply}
			if !apply {
				if jsonOutput {
					return writeJSON(command, result)
				}
				_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=true mount=%s sessions=%d\n", mount, len(states))
				return err
			}
			if err := os.MkdirAll(mount, 0o700); err != nil {
				return err
			}
			filesystem := mountfs.New()
			ctx, cancel := context.WithCancel(command.Context())
			defer cancel()
			closers := make([]io.Closer, 0)
			known := make(map[string]uint64)
			load := func() error {
				states, err := vfs.DiscoverSessionStates(store)
				if err != nil {
					return err
				}
				for _, state := range states {
					if known[state.SessionID] == state.Generation {
						continue
					}
					managed, resolver, err := openManagedSession(ctx, store, state)
					if err != nil {
						return err
					}
					closers = append(closers, resolver)
					if err := filesystem.UpsertSession(state.SessionID, managed); err != nil {
						return err
					}
					known[state.SessionID] = state.Generation
				}
				return nil
			}
			if err := load(); err != nil {
				return err
			}
			watcherDone := make(chan struct{})
			watcherErrors := make(chan error, 1)
			go func() {
				defer close(watcherDone)
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if err := load(); err != nil {
							watcherErrors <- err
							cancel()
							return
						}
					}
				}
			}()
			mountErr := mountfs.Mount(ctx, mountfs.HostOptions{MountPoint: mount, Filesystem: filesystem, Foreground: foreground})
			cancel()
			<-watcherDone
			for _, closer := range closers {
				_ = closer.Close()
			}
			select {
			case watcherErr := <-watcherErrors:
				return watcherErr
			default:
				return mountErr
			}
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().BoolVar(&apply, "apply", false, "Start the filesystem host")
	command.Flags().BoolVar(&foreground, "foreground", true, "Keep the FUSE host in the foreground")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output for dry-run")
	return command
}

func newFSMigrateCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var mountPoint string
	var mountWait time.Duration
	var apply bool
	var jsonOutput bool
	var compatibility compatibilityFlags
	command := &cobra.Command{
		Use:   "migrate <session-id>",
		Short: "Shadow and optionally route an eligible session to the mounted filesystem",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			session, manifest, resolver, view, err := openFoldView(home, store, args[0])
			if err != nil {
				return err
			}
			defer resolver.Close()
			if !session.Archived {
				return errors.New("only archived sessions are eligible for filesystem migration")
			}
			shadow, err := fsctl.Shadow(command.Context(), session.RolloutPath, view, fsctl.ShadowOptions{RandomReads: 10000, Seed: 1})
			if err != nil {
				return err
			}
			mount := defaultMountPoint(home, mountPoint)
			target := filepath.Join(mount, session.ID+".jsonl")
			native := vfs.NativeFile{Path: session.RolloutPath, Bytes: shadow.Bytes, SHA256: shadow.SHA256}
			result := FSMigrateResult{SessionID: session.ID, Native: native, Target: target, Shadow: shadow, DryRun: !apply}
			if apply {
				if err := requireStorageHealth(command.Context(), store); err != nil {
					return err
				}
				compatibilityResult, err := evaluateCompatibility(command.Context(), store, compatibility)
				if err != nil {
					return err
				}
				if len(compatibilityResult.DetectionErrors) != 0 || !compatibilityResult.Evaluation.Approved {
					return errors.New("installed Codex client versions are not covered by compatibility contracts")
				}
				if info, err := os.Stat(mount); err != nil || !info.IsDir() {
					return errors.New("filesystem mount point is not available")
				}
				if _, err := vfs.OpenSession(command.Context(), vfs.SessionOptions{Root: store, ManifestPath: fold.ManifestPath(store, session.ID), Manifest: manifest, Reader: resolver, NativeSnapshot: native}); err != nil {
					return err
				}
				targetFile, err := waitForTarget(command.Context(), target, mountWait)
				if err != nil {
					return fmt.Errorf("verify mounted target: %w", err)
				}
				if targetFile.Bytes != shadow.Bytes || targetFile.SHA256 != shadow.SHA256 {
					return errors.New("mounted target differs from the shadow-verified native session")
				}
				if _, err := codex.RouteSession(command.Context(), codex.RouteOptions{CodexHome: home, SessionID: session.ID, ExpectedPath: session.RolloutPath, Target: codex.RouteTarget{Path: target, Bytes: targetFile.Bytes, SHA256: targetFile.SHA256}}); err != nil {
					return err
				}
				result.Routed = true
				result.DryRun = false
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s shadow=%t dry_run=%t routed=%t target=%s\n", result.SessionID, result.Shadow.Verified, result.DryRun, result.Routed, result.Target)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().DurationVar(&mountWait, "mount-wait", 5*time.Second, "Maximum wait for the mounted session target")
	command.Flags().BoolVar(&apply, "apply", false, "Enroll and route the session after all gates pass")
	addCompatibilityFlags(command, &compatibility)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSRollbackCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var targetPath string
	var apply bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "rollback <session-id>",
		Short: "Route a managed session to a verified native file containing its latest visible bytes",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			state, err := managedState(store, args[0])
			if err != nil {
				return err
			}
			sessions, err := codex.LoadSessions(home)
			if err != nil {
				return err
			}
			current, err := findSession(sessions, args[0])
			if err != nil {
				return err
			}
			if targetPath == "" {
				targetPath = filepath.Join(store, "fs", "sessions", state.SessionID, "fallback-current.jsonl")
			}
			result := FSRollbackResult{SessionID: state.SessionID, From: current.RolloutPath, Target: vfs.NativeFile{Path: filepath.Clean(targetPath)}, DryRun: !apply}
			if apply {
				managed, resolver, err := openManagedSession(command.Context(), store, state)
				if err != nil {
					return err
				}
				defer resolver.Close()
				target, err := managed.MaterializeCurrent(command.Context(), filepath.Clean(targetPath), true)
				if err != nil {
					return err
				}
				if _, err := codex.RouteSession(command.Context(), codex.RouteOptions{CodexHome: home, SessionID: state.SessionID, ExpectedPath: current.RolloutPath, Target: codex.RouteTarget{Path: target.Path, Bytes: target.Bytes, SHA256: target.SHA256}}); err != nil {
					return err
				}
				result.Target = target
				result.Routed = true
				result.DryRun = false
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s dry_run=%t routed=%t from=%s target=%s\n", result.SessionID, result.DryRun, result.Routed, result.From, result.Target.Path)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&targetPath, "to", "", "Native rollback target; defaults to the managed session directory")
	command.Flags().BoolVar(&apply, "apply", false, "Materialize current bytes and update the Codex route")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSCompactCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var idleFor time.Duration
	var apply bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "compact <session-id>",
		Short: "Fold the latest visible bytes into a new verified immutable generation",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			state, err := managedState(store, args[0])
			if err != nil {
				return err
			}
			result := FSCompactResult{SessionID: state.SessionID, CurrentGeneration: state.Generation, NextGeneration: state.Generation + 1, DryRun: !apply}
			if apply {
				managed, resolver, err := openManagedSession(command.Context(), store, state)
				if err != nil {
					return err
				}
				defer resolver.Close()
				var preparedResolver *pack.Resolver
				defer func() {
					if preparedResolver != nil {
						_ = preparedResolver.Close()
					}
				}()
				compact, err := managed.Compact(command.Context(), vfs.CompactOptions{IdleFor: idleFor, Prepare: func(ctx context.Context, current vfs.NativeFile, generation uint64) (vfs.PreparedGeneration, error) {
					currentManifest, err := fold.LoadManifestPath(state.ManifestPath)
					if err != nil {
						return vfs.PreparedGeneration{}, err
					}
					manifestPath := filepath.Join(store, "manifests", "generations", state.SessionID, fmt.Sprintf("%020d.json", generation))
					options := fold.FoldOptions{
						StoreDir: store, ManifestPathOverride: manifestPath, Apply: true, Overwrite: true,
						FieldThreshold: currentManifest.Settings.FieldThreshold, MaxJSONLineBytes: currentManifest.Settings.MaxJSONLineBytes,
						CDC: cdc.Options{MinBytes: currentManifest.Settings.CDCMinBytes, AverageBytes: currentManifest.Settings.CDCAverageBytes, MaxBytes: currentManifest.Settings.CDCMaxBytes},
					}
					if _, err := fold.Fold(ctx, codex.Session{ID: state.SessionID, Title: currentManifest.Session.Title, CWD: currentManifest.Session.CWD, RolloutPath: current.Path, Archived: true}, options); err != nil {
						return vfs.PreparedGeneration{}, err
					}
					if _, err := pack.Build(ctx, store, pack.BuildOptions{}); err != nil {
						return vfs.PreparedGeneration{}, err
					}
					manifest, err := fold.LoadManifestPath(manifestPath)
					if err != nil {
						return vfs.PreparedGeneration{}, err
					}
					preparedResolver, err = pack.Open(store, pack.OpenOptions{})
					if err != nil {
						return vfs.PreparedGeneration{}, err
					}
					view, err := vfs.NewView(manifest, preparedResolver)
					if err != nil {
						return vfs.PreparedGeneration{}, err
					}
					return vfs.PreparedGeneration{ManifestPath: manifestPath, Manifest: manifest, View: view}, nil
				}})
				if err != nil {
					return err
				}
				result.NextGeneration = compact.Generation
				result.Bytes = compact.Bytes
				result.SHA256 = compact.SHA256
				result.DryRun = false
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s dry_run=%t generation=%d->%d bytes=%s sha256=%s\n", result.SessionID, result.DryRun, result.CurrentGeneration, result.NextGeneration, formatBytes(result.Bytes), result.SHA256)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().DurationVar(&idleFor, "idle-for", 0, "Minimum stable time before compaction")
	command.Flags().BoolVar(&apply, "apply", false, "Commit the new compacted generation")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSRecoverCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var all bool
	var apply bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "recover [session-id]",
		Short: "Inspect or recover interrupted managed session operations",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) == 0 && !all {
				return errors.New("provide a session ID or --all")
			}
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			states, err := vfs.DiscoverSessionStates(store)
			if err != nil {
				return err
			}
			selected := make([]vfs.SessionState, 0)
			for _, state := range states {
				if all || state.SessionID == args[0] {
					selected = append(selected, state)
				}
			}
			if !all && len(selected) == 0 {
				return fmt.Errorf("managed session not found: %s", args[0])
			}
			result := FSRecoverResult{DryRun: !apply}
			for _, state := range selected {
				result.SessionIDs = append(result.SessionIDs, state.SessionID)
				if !apply {
					continue
				}
				managed, resolver, err := openManagedSession(command.Context(), store, state)
				if err != nil {
					return err
				}
				if err := managed.Recover(command.Context()); err != nil {
					_ = resolver.Close()
					return err
				}
				_ = resolver.Close()
				result.Recovered++
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t selected=%d recovered=%d\n", result.DryRun, len(result.SessionIDs), result.Recovered)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&all, "all", false, "Recover every managed session")
	command.Flags().BoolVar(&apply, "apply", false, "Apply deterministic journal recovery")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func addCompatibilityFlags(command *cobra.Command, flags *compatibilityFlags) {
	command.Flags().StringVar(&flags.contractsPath, "contracts", "", "Compatibility contract directory; defaults to <store>/compatibility")
	command.Flags().StringVar(&flags.cliPath, "cli", "codex", "Codex CLI path, or 'none' to skip CLI evaluation")
	defaultDesktop := "none"
	if runtime.GOOS == "darwin" {
		defaultDesktop = "/Applications/ChatGPT.app"
	}
	command.Flags().StringVar(&flags.desktopPath, "desktop-app", defaultDesktop, "Codex desktop application path, or 'none' to skip desktop evaluation")
}

func evaluateCompatibility(ctx context.Context, store string, flags compatibilityFlags) (FSCompatibilityResult, error) {
	contractsPath := flags.contractsPath
	if contractsPath == "" {
		contractsPath = filepath.Join(store, "compatibility")
	}
	contracts, err := compat.LoadAll(contractsPath)
	if err != nil {
		return FSCompatibilityResult{}, err
	}
	result := FSCompatibilityResult{Contracts: len(contracts)}
	if flags.cliPath != "none" {
		binary := flags.cliPath
		if !strings.ContainsRune(binary, filepath.Separator) {
			resolved, err := exec.LookPath(binary)
			if err != nil {
				result.DetectionErrors = append(result.DetectionErrors, "cli: "+err.Error())
			} else {
				binary = resolved
			}
		}
		if len(result.DetectionErrors) == 0 {
			client, err := compat.DetectCLIVersion(ctx, binary)
			if err != nil {
				result.DetectionErrors = append(result.DetectionErrors, "cli: "+err.Error())
			} else {
				result.Installed = append(result.Installed, client)
			}
		}
	}
	if flags.desktopPath != "none" {
		if _, err := os.Stat(flags.desktopPath); err != nil {
			result.DetectionErrors = append(result.DetectionErrors, "desktop: "+err.Error())
		} else {
			client, err := compat.DetectDesktopVersion(ctx, flags.desktopPath)
			if err != nil {
				result.DetectionErrors = append(result.DetectionErrors, "desktop: "+err.Error())
			} else {
				result.Installed = append(result.Installed, client)
			}
		}
	}
	result.Evaluation = compat.Evaluate(result.Installed, contracts)
	if len(result.Installed) == 0 {
		result.Evaluation = compat.Evaluation{Approved: false, Quarantine: true}
	}
	return result, nil
}

func openFoldView(home string, store string, sessionID string) (codex.Session, fold.Manifest, *pack.Resolver, *vfs.View, error) {
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return codex.Session{}, fold.Manifest{}, nil, nil, err
	}
	session, err := findSession(sessions, sessionID)
	if err != nil {
		return codex.Session{}, fold.Manifest{}, nil, nil, err
	}
	manifest, err := fold.LoadManifest(store, session.ID)
	if err != nil {
		return codex.Session{}, fold.Manifest{}, nil, nil, err
	}
	resolver, err := pack.Open(store, pack.OpenOptions{})
	if err != nil {
		return codex.Session{}, fold.Manifest{}, nil, nil, err
	}
	view, err := vfs.NewView(manifest, resolver)
	if err != nil {
		_ = resolver.Close()
		return codex.Session{}, fold.Manifest{}, nil, nil, err
	}
	return session, manifest, resolver, view, nil
}

func openManagedSession(ctx context.Context, store string, state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
	manifest, err := fold.LoadManifestPath(state.ManifestPath)
	if err != nil {
		return nil, nil, err
	}
	resolver, err := pack.Open(store, pack.OpenOptions{})
	if err != nil {
		return nil, nil, err
	}
	managed, err := vfs.OpenSession(ctx, vfs.SessionOptions{Root: store, ManifestPath: state.ManifestPath, Manifest: manifest, Reader: resolver, NativeSnapshot: state.NativeSnapshot})
	if err != nil {
		_ = resolver.Close()
		return nil, nil, err
	}
	return managed, resolver, nil
}

func managedState(store string, sessionID string) (vfs.SessionState, error) {
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		return vfs.SessionState{}, err
	}
	for _, state := range states {
		if state.SessionID == sessionID {
			return state, nil
		}
	}
	return vfs.SessionState{}, fmt.Errorf("managed session not found: %s", sessionID)
}

func requireStorageHealth(ctx context.Context, store string) error {
	packReport, err := pack.Doctor(ctx, store)
	if err != nil {
		return err
	}
	if packReport.IssueCount != 0 {
		return fmt.Errorf("pack doctor reported %d issues", packReport.IssueCount)
	}
	foldReport, err := fold.Doctor(ctx, store)
	if err != nil {
		return err
	}
	if foldReport.IssueCount != 0 {
		return fmt.Errorf("fold doctor reported %d issues", foldReport.IssueCount)
	}
	return nil
}

func fsDoctor(ctx context.Context, home string, store string, mount string) fsctl.DoctorReport {
	checks := []fsctl.Check{
		{Component: fsctl.ComponentDaemon, Run: func(context.Context) error { return errors.New("managed service lifecycle is not installed") }},
		{Component: fsctl.ComponentMount, Run: func(context.Context) error {
			info, err := os.Stat(mount)
			if err != nil || !info.IsDir() {
				return errors.New("filesystem mount point is unavailable")
			}
			return nil
		}},
		{Component: fsctl.ComponentPack, Run: func(ctx context.Context) error {
			report, err := pack.Doctor(ctx, store)
			if err != nil {
				return err
			}
			if report.IssueCount != 0 {
				return fmt.Errorf("pack doctor reported %d issues", report.IssueCount)
			}
			return nil
		}},
		{Component: fsctl.ComponentManifest, Run: func(ctx context.Context) error {
			report, err := fold.Doctor(ctx, store)
			if err != nil {
				return err
			}
			if report.IssueCount != 0 {
				return fmt.Errorf("fold doctor reported %d issues", report.IssueCount)
			}
			return nil
		}},
	}
	states, stateErr := vfs.DiscoverSessionStates(store)
	stateCheck := func(kind string) fsctl.Check {
		return fsctl.Check{Component: kind, Run: func(context.Context) error {
			if stateErr != nil {
				return stateErr
			}
			for _, state := range states {
				paths := []string{state.DeltaPath}
				if kind == fsctl.ComponentBacking && state.BackingPath != "" {
					paths = []string{state.BackingPath}
				}
				for _, path := range paths {
					if _, err := os.Stat(path); err != nil {
						return err
					}
				}
			}
			return nil
		}}
	}
	checks = append(checks, stateCheck(fsctl.ComponentDelta), stateCheck(fsctl.ComponentBacking))
	checks = append(checks,
		fsctl.Check{Component: fsctl.ComponentRoute, Run: func(context.Context) error {
			sessions, err := codex.LoadSessions(home)
			if err != nil {
				return err
			}
			for _, session := range sessions {
				if _, err := os.Stat(session.RolloutPath); err != nil {
					return fmt.Errorf("session %s route: %w", session.ID, err)
				}
			}
			return nil
		}},
		fsctl.Check{Component: fsctl.ComponentFallback, Run: func(context.Context) error {
			if stateErr != nil {
				return stateErr
			}
			for _, state := range states {
				if _, err := os.Stat(state.NativeSnapshot.Path); err != nil {
					return err
				}
			}
			return nil
		}},
		fsctl.Check{Component: fsctl.ComponentJournal, Run: func(context.Context) error {
			if stateErr != nil {
				return stateErr
			}
			for _, state := range states {
				path := filepath.Join(store, "fs", "sessions", state.SessionID, "journal.jsonl")
				if file, err := os.Open(path); err == nil {
					_ = file.Close()
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			return nil
		}},
		fsctl.Check{Component: fsctl.ComponentClient, Run: func(context.Context) error { return errors.New("run fs compatibility with explicit client contracts") }},
	)
	return fsctl.Doctor(ctx, checks)
}

func defaultMountPoint(home string, explicit string) string {
	if explicit != "" {
		return filepath.Clean(explicit)
	}
	return filepath.Join(home, "fold-fs")
}

func waitForTarget(ctx context.Context, target string, timeout time.Duration) (vfs.NativeFile, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		file, err := hashPath(target)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return vfs.NativeFile{}, err
		}
		if time.Now().After(deadline) {
			return vfs.NativeFile{}, os.ErrNotExist
		}
		select {
		case <-ctx.Done():
			return vfs.NativeFile{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func hashPath(path string) (vfs.NativeFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return vfs.NativeFile{}, err
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return vfs.NativeFile{}, copyErr
	}
	if closeErr != nil {
		return vfs.NativeFile{}, closeErr
	}
	return vfs.NativeFile{Path: path, Bytes: bytesRead, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}
