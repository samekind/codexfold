package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/samekind/codexfold/internal/cdc"
	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/compat"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/fsctl"
	"github.com/samekind/codexfold/internal/fskitproto"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/service"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
	"github.com/spf13/cobra"
)

type FSMigrateResult struct {
	SessionID string                      `json:"session_id"`
	Native    vfs.NativeFile              `json:"native"`
	Target    string                      `json:"target"`
	Shadow    fsctl.ShadowResult          `json:"shadow"`
	DryRun    bool                        `json:"dry_run"`
	Routed    bool                        `json:"routed"`
	Storage   *storage.MutationAccounting `json:"storage,omitempty"`
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
	Frontend        string `json:"frontend"`
	ResourcePath    string `json:"resource_path,omitempty"`
	DryRun          bool   `json:"dry_run"`
}

type FSRollbackResult struct {
	SessionID    string                      `json:"session_id"`
	From         string                      `json:"from"`
	Target       vfs.NativeFile              `json:"target"`
	RetiredState string                      `json:"retired_state,omitempty"`
	DryRun       bool                        `json:"dry_run"`
	Routed       bool                        `json:"routed"`
	Storage      *storage.MutationAccounting `json:"storage,omitempty"`
}

type FSCompactResult struct {
	SessionID         string                      `json:"session_id"`
	CurrentGeneration uint64                      `json:"current_generation"`
	NextGeneration    uint64                      `json:"next_generation"`
	Bytes             int64                       `json:"bytes,omitempty"`
	SHA256            string                      `json:"sha256,omitempty"`
	DryRun            bool                        `json:"dry_run"`
	Storage           *storage.MutationAccounting `json:"storage,omitempty"`
}

type FSRecoverResult struct {
	SessionIDs []string `json:"session_ids"`
	Recovered  int      `json:"recovered"`
	DryRun     bool     `json:"dry_run"`
}

type FSNativeValidationResult struct {
	Healthy bool                           `json:"healthy"`
	Report  mountfs.NativePreflightReport  `json:"report"`
	Issues  []mountfs.NativePreflightIssue `json:"issues,omitempty"`
}

type compatibilityFlags struct {
	contractsPath string
	cliPath       string
	desktopPath   string
}

var mountHealthProbe = service.ProbeMount

func newFSCommand() *cobra.Command {
	command := &cobra.Command{Use: "fs", Short: "Operate the transparent session filesystem"}
	command.AddCommand(newFSStatusCommand())
	command.AddCommand(newFSDoctorCommand())
	command.AddCommand(newFSValidateNativeCommand())
	command.AddCommand(newFSCompatibilityCommand())
	command.AddCommand(newFSCompatibilityImportCommand())
	command.AddCommand(newFSBenchmarkCommand())
	command.AddCommand(newFSServeCommand())
	command.AddCommand(newFSNativeSupervisorCommand())
	command.AddCommand(newFSMigrateCommand())
	command.AddCommand(newFSRollbackCommand())
	command.AddCommand(newFSCompactCommand())
	command.AddCommand(newFSRecoverCommand())
	command.AddCommand(newFSEnrollCommand())
	command.AddCommand(newFSRepairRolloutCommand())
	command.AddCommand(newFSReconcileRolloutCommand())
	command.AddCommand(newFSNamespaceCommand())
	command.AddCommand(newFSServiceCommand())
	return command
}

func newFSValidateNativeCommand() *cobra.Command {
	var codexHome string
	var nativeRoot string
	var auditAll bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "validate-native",
		Short: "Validate active native rollout JSONL before writer routing",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			root := nativeRoot
			if root == "" {
				root = filepath.Join(home, "fold-native")
			}
			result := FSNativeValidationResult{Healthy: true}
			if auditAll {
				audit, err := mountfs.AuditNativeWriterRollouts(command.Context(), root)
				if err != nil {
					return err
				}
				result.Report = audit.NativePreflightReport
				result.Issues = audit.Issues
				result.Healthy = len(result.Issues) == 0
			} else {
				filesystem := mountfs.NewCanonical()
				filesystem.SetNativeRoot(root)
				result.Report, err = filesystem.ValidateNativeWriterRollouts(command.Context())
				if err != nil {
					result.Healthy = false
					result.Issues = []mountfs.NativePreflightIssue{{Message: err.Error()}}
				}
			}
			if jsonOutput {
				if err := writeJSON(command, result); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "healthy=%t files=%d bytes=%d validated=%d incremental=%d cached=%d issues=%d\n", result.Healthy, result.Report.Files, result.Report.Bytes, result.Report.ValidatedFiles, result.Report.IncrementalFiles, result.Report.CachedFiles, len(result.Issues)); err != nil {
					return err
				}
			}
			if !result.Healthy {
				return fmt.Errorf("native rollout validation failed with %d issue(s)", len(result.Issues))
			}
			return nil
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&nativeRoot, "native-root", "", "Native rollout root; defaults to <codex-home>/fold-native")
	command.Flags().BoolVar(&auditAll, "audit-all", false, "Bypass the cache and report every invalid active rollout")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSStatusCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Report the highest verified filesystem capability",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			status, err := fsctl.NewStatus(verifiedCapability(), runtime.GOOS)
			if err != nil {
				return err
			}
			status.Storage, err = storage.Scan(command.Context(), storage.Options{StoreDir: store, AllowMetadataIssues: true})
			if err != nil {
				return err
			}
			status.StorageLimits, err = storage.LoadLimits(store)
			if err != nil {
				return err
			}
			status.AvailableBytes, err = storage.AvailableBytes(store)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, status)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "capability=%s platform=%s logical=%s physical=%s available=%s\n", status.Capability, status.Platform, formatBytes(status.Storage.LogicalSessionBytes), formatBytes(status.Storage.TotalPhysicalBytes), formatBytes(status.AvailableBytes))
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSDoctorCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var mountPoint string
	var definitionPath string
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
			report := fsDoctor(command.Context(), home, store, mount, definitionPath)
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
	addServiceDefinitionFlags(command, &definitionPath)
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
			states, err := vfs.DiscoverSessionStates(store)
			if err != nil {
				return err
			}
			var managedState *vfs.SessionState
			for index := range states {
				if states[index].SessionID == args[0] {
					state := states[index]
					managedState = &state
					break
				}
			}

			var nativePath string
			var virtual fsctl.Readable
			var closeVirtual func() error
			var cleanup func()
			if managedState != nil {
				managed, resolver, openErr := openManagedSession(command.Context(), store, *managedState)
				if openErr != nil {
					return openErr
				}
				defer resolver.Close()
				reader, openErr := managed.OpenReader()
				if openErr != nil {
					return openErr
				}
				closeVirtual = reader.Close
				defer func() { _ = closeVirtual() }()

				benchmarkDir, openErr := os.MkdirTemp("", "codexfold-benchmark-")
				if openErr != nil {
					return openErr
				}
				cleanup = func() { _ = os.RemoveAll(benchmarkDir) }
				defer cleanup()
				materialized, materializeErr := managed.MaterializeCurrent(command.Context(), filepath.Join(benchmarkDir, "visible.jsonl"), false)
				if materializeErr != nil {
					return materializeErr
				}
				nativePath = materialized.Path
				virtual = reader
			} else {
				session, manifest, resolver, view, openErr := openFoldView(home, store, args[0])
				if openErr != nil {
					return openErr
				}
				defer resolver.Close()
				if manifest.Source.SHA256 == "" {
					return errors.New("manifest source digest is missing")
				}
				nativePath = session.RolloutPath
				virtual = view
			}
			report, err := fsctl.Benchmark(command.Context(), nativePath, virtual, options)
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
	command.Flags().BoolVar(&options.BypassOSCache, "bypass-os-cache", false, "Request OS cache bypass for the native and packed reads")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSServeCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var mountPoint string
	var apply bool
	var foreground bool
	var canonicalNamespace bool
	var nativeRoot string
	var frontend string
	var nativeFSKitSocket string
	var nativeFSKitResource string
	var operationTracePath string
	var enrollmentInterval time.Duration
	var enrollmentStableFor time.Duration
	var enrollmentBatchSize int
	var enrollmentCanary bool
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
			if apply {
				if err := requireFilesystemActivationAllowed(home); err != nil {
					return err
				}
			}
			if canonicalNamespace {
				if nativeRoot == "" || !filepath.IsAbs(nativeRoot) {
					return errors.New("canonical namespace requires an absolute native root")
				}
				nativeRoot = filepath.Clean(nativeRoot)
			}
			if frontend != "fuse" && frontend != "native-fskit" {
				return errors.New("filesystem frontend must be fuse or native-fskit")
			}
			if frontend == "native-fskit" {
				if runtime.GOOS != "darwin" {
					return errors.New("native-fskit frontend is available only on macOS")
				}
				if !canonicalNamespace {
					return errors.New("native-fskit frontend requires the canonical namespace")
				}
			}
			if enrollmentInterval < 0 || enrollmentStableFor < 0 || enrollmentBatchSize < 0 {
				return errors.New("enrollment timing and batch values cannot be negative")
			}
			if enrollmentInterval > 0 {
				if !canonicalNamespace {
					return errors.New("periodic enrollment requires the canonical namespace")
				}
				if enrollmentStableFor <= 0 || enrollmentBatchSize <= 0 {
					return errors.New("periodic enrollment requires a positive stable window and batch size")
				}
			}
			if enrollmentCanary && enrollmentInterval <= 0 {
				return errors.New("enrollment canary requires periodic enrollment")
			}
			store := resolveFoldStore(home, storeDir)
			mount := defaultMountPoint(home, mountPoint)
			states, err := vfs.DiscoverSessionStates(store)
			if err != nil {
				return err
			}
			if nativeFSKitResource == "" {
				nativeFSKitResource = filepath.Join(store, "fs", "native-fskit")
			}
			if nativeFSKitSocket == "" {
				nativeFSKitSocket = defaultNativeFSKitSocket(home, nativeFSKitResource)
			}
			result := FSServeResult{MountPoint: mount, ManagedSessions: len(states), Frontend: frontend, DryRun: !apply}
			if frontend == "native-fskit" {
				result.ResourcePath = nativeFSKitResource
			}
			if !apply {
				if jsonOutput {
					return writeJSON(command, result)
				}
				_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=true frontend=%s mount=%s sessions=%d resource=%s\n", frontend, mount, len(states), result.ResourcePath)
				return err
			}
			processLock, err := service.AcquireProcessLock(filepath.Join(store, "fs", "service.lock"))
			if err != nil {
				return err
			}
			defer processLock.Close()
			if canonicalNamespace {
				for _, state := range states {
					if _, err := recoverInterruptedCanonicalMigration(home, store, nativeRoot, state); err != nil {
						return err
					}
				}
				states, err = vfs.DiscoverSessionStates(store)
				if err != nil {
					return err
				}
				result.ManagedSessions = len(states)
			}
			var operationRecorder func(string)
			if operationTracePath != "" {
				recorder, closer, err := newOperationRecorder(operationTracePath)
				if err != nil {
					return err
				}
				operationRecorder = recorder
				defer closer.Close()
			}
			if canonicalNamespace {
				for _, directory := range []string{"sessions", "archived_sessions"} {
					if err := os.MkdirAll(filepath.Join(nativeRoot, directory), 0o700); err != nil {
						return err
					}
				}
			}
			filesystem := mountfs.New()
			if canonicalNamespace {
				filesystem = mountfs.NewCanonical()
				filesystem.SetNativeRoot(nativeRoot)
				if frontend == "native-fskit" {
					filesystem.SetNativeNamespaceRefreshMount(mount)
				}
				if err := filesystem.RecoverNativeAppendTransactions(); err != nil {
					return fmt.Errorf("recover native append transactions: %w", err)
				}
				if _, err := filesystem.ValidateNativeWriterRollouts(command.Context()); err != nil {
					return fmt.Errorf("validate native writer rollouts: %w", err)
				}
			}
			ctx, cancel := context.WithCancel(command.Context())
			defer cancel()
			var nativeWatcherDone chan error
			if frontend == "native-fskit" {
				nativeWatcherDone = make(chan error, 1)
				go func() {
					err := filesystem.WatchNativeNamespace(ctx)
					nativeWatcherDone <- err
					if err != nil && !errors.Is(err, context.Canceled) {
						cancel()
					}
				}()
			}
			var enrollmentDone chan struct{}
			if enrollmentInterval > 0 {
				enrollmentDone = make(chan struct{})
				flags := enrollmentFlags{
					codexHome: home, storeDir: store, mountPoint: mount, nativeRoot: nativeRoot,
					stableFor: enrollmentStableFor, batchSize: enrollmentBatchSize,
					canonicalNamespace: canonicalNamespace, canary: enrollmentCanary,
				}
				go func() {
					defer close(enrollmentDone)
					runPeriodicEnrollment(ctx, flags, enrollmentInterval, func(result FSEnrollmentApplyResult, cycleErr error) {
						if cycleErr != nil {
							if !errors.Is(cycleErr, context.Canceled) {
								_, _ = fmt.Fprintf(command.ErrOrStderr(), "enrollment cycle failed: %v\n", cycleErr)
							}
							return
						}
						if len(result.Plan.Selected) == 0 && result.Apply.Applied == 0 {
							return
						}
						_, _ = fmt.Fprintf(command.ErrOrStderr(), "enrollment cycle sessions=%d selected=%d applied=%d\n", len(result.Plan.Decisions), len(result.Plan.Selected), result.Apply.Applied)
					})
				}()
			}
			known := make(map[string]uint64)
			knownRoutes := make(map[string]string)
			knownPacks := make(map[string]string)
			var loadMu sync.Mutex
			openState := func(state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
				managed, resolver, err := openManagedSession(ctx, store, state)
				if err != nil {
					return nil, nil, err
				}
				return managed, resolver, nil
			}
			filesystem.SetOwnedSessionLoader(func(sessionID string) (*vfs.Session, io.Closer, error) {
				loadMu.Lock()
				defer loadMu.Unlock()
				states, err := vfs.DiscoverSessionStates(store)
				if err != nil {
					return nil, nil, err
				}
				for _, state := range states {
					if state.SessionID == sessionID {
						managed, resolver, err := openState(state)
						if err == nil {
							known[state.SessionID] = state.Generation
							knownPacks[state.SessionID] = resolver.Generation()
						}
						return managed, resolver, err
					}
				}
				return nil, nil, os.ErrNotExist
			})
			load := func() error {
				loadMu.Lock()
				defer loadMu.Unlock()
				states, err := vfs.DiscoverSessionStates(store)
				if err != nil {
					return err
				}
				currentPack, err := pack.CurrentGeneration(store)
				if err != nil {
					return err
				}
				routes := make(map[string]string)
				if canonicalNamespace {
					routes, err = discoverCanonicalRoutes(home, mount, store, states, codex.LoadSessions)
					if err != nil {
						return err
					}
				}
				seen := make(map[string]struct{}, len(states))
				for _, state := range states {
					seen[state.SessionID] = struct{}{}
					if canonicalNamespace {
						route, exists := routes[state.SessionID]
						handled, err := syncCanonicalRetirement(store, home, nativeRoot, filesystem, state, route, exists, known, knownRoutes, knownPacks, currentPack, openState)
						if err != nil {
							return err
						}
						if handled {
							continue
						}
						if !exists {
							if _, mounted := known[state.SessionID]; mounted {
								if err := filesystem.RemoveSession(state.SessionID); err != nil && !errors.Is(err, os.ErrNotExist) {
									return err
								}
								delete(known, state.SessionID)
								delete(knownRoutes, state.SessionID)
								delete(knownPacks, state.SessionID)
							}
							continue
						}
						generation, generationKnown := known[state.SessionID]
						if generationKnown && generation == state.Generation && knownPacks[state.SessionID] == currentPack {
							if knownRoutes[state.SessionID] == route {
								continue
							}
							if err := filesystem.MoveSessionAt(state.SessionID, route); err != nil {
								return err
							}
							knownRoutes[state.SessionID] = route
							if err := writeMountAcknowledgement(store, state.SessionID, state.Generation, route); err != nil {
								return err
							}
							continue
						}
						managed, resolver, err := openState(state)
						if err != nil {
							return err
						}
						if err := filesystem.UpsertSessionAtOwned(state.SessionID, route, managed, resolver); err != nil {
							return err
						}
						known[state.SessionID] = state.Generation
						knownRoutes[state.SessionID] = route
						knownPacks[state.SessionID] = resolver.Generation()
						if err := writeMountAcknowledgement(store, state.SessionID, state.Generation, route); err != nil {
							return err
						}
						continue
					}
					if known[state.SessionID] == state.Generation && knownPacks[state.SessionID] == currentPack {
						continue
					}
					managed, resolver, err := openState(state)
					if err != nil {
						return err
					}
					if err := filesystem.UpsertSessionOwned(state.SessionID, managed, resolver); err != nil {
						return err
					}
					known[state.SessionID] = state.Generation
					knownPacks[state.SessionID] = resolver.Generation()
				}
				for sessionID := range known {
					if _, exists := seen[sessionID]; exists {
						continue
					}
					if err := filesystem.RemoveSession(sessionID); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
					delete(known, sessionID)
					delete(knownRoutes, sessionID)
					delete(knownPacks, sessionID)
				}
				return nil
			}
			if err := load(); err != nil {
				return err
			}
			storageMaintenanceDone := startStorageMaintenance(ctx, command.ErrOrStderr(), store, startupStorageGC)
			runtimeMemoryMaintenanceDone := startRuntimeMemoryMaintenance(ctx, filesystem)
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
			var mountErr error
			if frontend == "native-fskit" {
				mountErr = mountfs.ServeNativeFSKit(ctx, filesystem, mountfs.NativeFSKitServerOptions{
					SocketPath: nativeFSKitSocket, ResourcePath: nativeFSKitResource, Recorder: operationRecorder,
					PrewarmSharedMemoryWindows: 4,
				})
			} else {
				mountErr = mountfs.Mount(ctx, mountfs.HostOptions{MountPoint: mount, Filesystem: filesystem, Foreground: foreground, OperationRecorder: operationRecorder})
			}
			cancel()
			<-watcherDone
			<-storageMaintenanceDone
			<-runtimeMemoryMaintenanceDone
			if enrollmentDone != nil {
				<-enrollmentDone
			}
			var nativeWatcherErr error
			if nativeWatcherDone != nil {
				nativeWatcherErr = <-nativeWatcherDone
				if errors.Is(nativeWatcherErr, context.Canceled) {
					nativeWatcherErr = nil
				}
			}
			sessionCloseErr := filesystem.CloseSessions()
			select {
			case watcherErr := <-watcherErrors:
				return errors.Join(watcherErr, sessionCloseErr)
			default:
				return errors.Join(mountErr, nativeWatcherErr, sessionCloseErr)
			}
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().BoolVar(&apply, "apply", false, "Start the filesystem host")
	command.Flags().BoolVar(&foreground, "foreground", true, "Keep the FUSE host in the foreground")
	command.Flags().BoolVar(&canonicalNamespace, "canonical-namespace", false, "Expose sessions and archived_sessions as a shared virtual namespace")
	command.Flags().StringVar(&nativeRoot, "native-root", "", "Backing root for unmanaged canonical session files")
	command.Flags().StringVar(&frontend, "frontend", "fuse", "Filesystem frontend: fuse or native-fskit")
	command.Flags().StringVar(&nativeFSKitSocket, "fskit-socket", "", "Native FSKit daemon Unix socket; defaults to a short per-home path in /private/tmp")
	command.Flags().StringVar(&nativeFSKitResource, "fskit-resource", "", "Native FSKit resource; defaults to the security-scoped <store>/fs/native-fskit directory")
	command.Flags().StringVar(&operationTracePath, "operation-trace", "", "Absolute path for sanitized FUSE operation names")
	command.Flags().DurationVar(&enrollmentInterval, "enrollment-interval", 0, "Periodic stable-session enrollment interval; zero disables the loop")
	command.Flags().DurationVar(&enrollmentStableFor, "enrollment-stable-for", time.Hour, "Required unchanged interval before periodic enrollment")
	command.Flags().IntVar(&enrollmentBatchSize, "enrollment-batch-size", 1, "Maximum sessions enrolled per periodic cycle")
	command.Flags().BoolVar(&enrollmentCanary, "enrollment-canary", false, "Allow periodic enrollment only in an explicitly isolated Codex home while capability remains preview")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output for dry-run")
	return command
}

func defaultNativeFSKitSocket(home string, resourcePath string) string {
	if fskitproto.UsesDirectoryResource(resourcePath) {
		return filepath.Join(filepath.Clean(resourcePath), "daemon.sock")
	}
	digest := sha256.Sum256([]byte(filepath.Clean(home)))
	userHome, err := os.UserHomeDir()
	if err == nil && runtime.GOOS == "darwin" {
		return filepath.Join(userHome, "Library", "Containers", "vip.jstar.codexfold.fskitprofileprobe.module", "Data", "tmp", fmt.Sprintf("cf-%s.sock", hex.EncodeToString(digest[:4])))
	}
	return filepath.Join("/private/tmp", fmt.Sprintf("codexfold-fskit-%d-%s.sock", os.Getuid(), hex.EncodeToString(digest[:4])))
}

func syncCanonicalRetirement(
	store string,
	home string,
	nativeRoot string,
	filesystem *mountfs.Filesystem,
	state vfs.SessionState,
	route string,
	routeExists bool,
	known map[string]uint64,
	knownRoutes map[string]string,
	knownPacks map[string]string,
	currentPack string,
	openState func(vfs.SessionState) (*vfs.Session, *pack.Resolver, error),
) (bool, error) {
	retirement, retiring, err := readRetirementRequest(store, state.SessionID)
	if err != nil {
		return false, err
	}
	if !retiring {
		return false, removeIfExists(filepath.Join(store, "fs", "sessions", state.SessionID, retirementAcknowledgementFilename))
	}
	reject := func(message string) (bool, error) {
		rejected := retirement
		rejected.Error = message
		return true, writeRetirementAcknowledgement(store, state.SessionID, rejected)
	}
	if !routeExists || retirement.Route != route {
		return reject("retirement request does not match the current session route")
	}
	generation := known[state.SessionID]
	if generation != state.Generation || knownRoutes[state.SessionID] != route || knownPacks[state.SessionID] != currentPack {
		managed, resolver, err := openState(state)
		if err != nil {
			return true, err
		}
		if err := filesystem.UpsertSessionAtOwned(state.SessionID, route, managed, resolver); err != nil {
			return true, err
		}
		generation = managed.State().Generation
		known[state.SessionID] = generation
		knownRoutes[state.SessionID] = route
		knownPacks[state.SessionID] = resolver.Generation()
	}
	if retirement.Generation != generation {
		return reject("retirement request generation does not match the current session state")
	}
	nativeTargetPath, err := canonicalNativeRoute(home, nativeRoot, filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(route, "/"))))
	if err != nil {
		return true, err
	}
	nativeTarget, targetErr := hashPath(nativeTargetPath)
	if targetErr != nil || nativeTarget.Bytes != retirement.Bytes || nativeTarget.SHA256 != retirement.SHA256 {
		return reject("native rollback target is unavailable or changed")
	}
	if err := filesystem.PreferNativeSession(state.SessionID); err != nil {
		return true, err
	}
	if err := writeRetirementAcknowledgement(store, state.SessionID, retirement); err != nil {
		return true, err
	}
	return true, nil
}

func newFSMigrateCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var mountPoint string
	var nativeRoot string
	var mountWait time.Duration
	var apply, canonicalNamespace, compatibilityCanary bool
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
			if apply {
				if err := requireFilesystemActivationAllowed(home); err != nil {
					return err
				}
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
			mount := defaultMountPoint(home, mountPoint)
			sourcePath := session.RolloutPath
			target := filepath.Join(mount, session.ID+".jsonl")
			if canonicalNamespace {
				if nativeRoot == "" {
					nativeRoot = filepath.Join(home, "fold-native")
				}
				sourcePath, err = canonicalNativeRoute(home, nativeRoot, session.RolloutPath)
				if err != nil {
					return err
				}
				target, err = canonicalMountRoute(home, mount, session.RolloutPath)
				if err != nil {
					return err
				}
			}
			if _, err := mountfs.ValidateNativeRollout(command.Context(), sourcePath); err != nil {
				return fmt.Errorf("native rollout is not eligible for transparent routing: %w", err)
			}
			shadow, err := fsctl.Shadow(command.Context(), sourcePath, view, fsctl.ShadowOptions{RandomReads: 10000, Seed: 1})
			if err != nil {
				return err
			}
			native := vfs.NativeFile{Path: sourcePath, Bytes: shadow.Bytes, SHA256: shadow.SHA256}
			result := FSMigrateResult{SessionID: session.ID, Native: native, Target: target, Shadow: shadow, DryRun: !apply}
			if apply {
				if err := requireStorageHealth(command.Context(), store); err != nil {
					return err
				}
				if compatibilityCanary {
					userHome, err := os.UserHomeDir()
					if err != nil {
						return err
					}
					if err := validateCompatibilityCanary(home, filepath.Join(userHome, ".codex"), store, canonicalNamespace, compatibility); err != nil {
						return err
					}
				} else {
					compatibilityResult, err := evaluateCompatibility(command.Context(), store, compatibility)
					if err != nil {
						return err
					}
					if len(compatibilityResult.DetectionErrors) != 0 || !compatibilityResult.Evaluation.Approved {
						return errors.New("installed Codex client versions are not covered by compatibility contracts")
					}
				}
				if err := mountHealthProbe(mount); err != nil {
					return fmt.Errorf("filesystem mount point is not healthy: %w", err)
				}
				projectedPersistent := int64(1 << 20)
				if canonicalNamespace {
					projectedPersistent += native.Bytes
				}
				storageAssessment, err := assessStoreMutation(command.Context(), store, storage.Projection{Operation: "fs-migrate", AdditionalPersistentBytes: projectedPersistent})
				if err != nil {
					return err
				}
				canonicalSource := ""
				canonicalRoute := ""
				if canonicalNamespace {
					if _, err := os.Stat(filepath.Join(store, "fs", "sessions", session.ID, "state.json")); err == nil {
						return errors.New("session is already managed")
					} else if !errors.Is(err, os.ErrNotExist) {
						return err
					}
					canonicalSource = native.Path
					canonicalRoute, err = canonicalNamespaceRoute(home, mount, session.RolloutPath)
					if err != nil {
						return err
					}
					retained, err := retainCanonicalSnapshot(command.Context(), store, session.ID, native, nil)
					if err != nil {
						return err
					}
					native = retained
					result.Native = retained
				}
				rollbackMigration := func(cause error) error {
					if !canonicalNamespace {
						if _, err := os.Stat(filepath.Join(store, "fs", "sessions", session.ID)); errors.Is(err, os.ErrNotExist) {
							return cause
						} else if err != nil {
							return errors.Join(cause, err)
						}
						if _, err := retireManagedState(store, session.ID); err != nil {
							return errors.Join(cause, err)
						}
						return cause
					}
					return rollbackCanonicalMigration(store, session.ID, canonicalSource, native.Path, cause)
				}
				managed, migrationLease, err := vfs.OpenSessionWithWriter(command.Context(), vfs.SessionOptions{Root: store, ManifestPath: fold.ManifestPath(store, session.ID), Manifest: manifest, Reader: resolver, NativeSnapshot: native})
				if err != nil {
					return rollbackMigration(err)
				}
				defer migrationLease.Close()
				if canonicalNamespace {
					if err := waitForMountAcknowledgement(command.Context(), store, session.ID, managed.State().Generation, canonicalRoute, mountWait); err != nil {
						return rollbackMigration(fmt.Errorf("wait for canonical mount acknowledgement: %w", err))
					}
					sessions, err := codex.LoadSessions(home)
					if err != nil {
						return rollbackMigration(err)
					}
					current, err := findSession(sessions, session.ID)
					if err != nil || filepath.Clean(current.RolloutPath) != filepath.Clean(session.RolloutPath) {
						return rollbackMigration(errors.New("canonical Codex route changed during migration"))
					}
					if _, err := waitForTargetMatch(command.Context(), target, vfs.NativeFile{Bytes: shadow.Bytes, SHA256: shadow.SHA256}, mountWait); err != nil {
						return rollbackMigration(fmt.Errorf("verify managed target before canonical cutover: %w", err))
					}
					if err := finalizeCanonicalSnapshotSource(canonicalSource, native); err != nil {
						return rollbackMigration(err)
					}
				}
				targetFile, err := waitForTarget(command.Context(), target, mountWait)
				if err != nil {
					return rollbackMigration(fmt.Errorf("verify mounted target: %w", err))
				}
				if targetFile.Bytes != shadow.Bytes || targetFile.SHA256 != shadow.SHA256 {
					return rollbackMigration(errors.New("mounted target differs from the shadow-verified native session"))
				}
				if !canonicalNamespace {
					if _, err := codex.RouteSession(command.Context(), codex.RouteOptions{CodexHome: home, SessionID: session.ID, ExpectedPath: session.RolloutPath, Target: codex.RouteTarget{Path: target, Bytes: targetFile.Bytes, SHA256: targetFile.SHA256}}); err != nil {
						return err
					}
				} else {
					sessions, err := codex.LoadSessions(home)
					if err != nil {
						return err
					}
					current, err := findSession(sessions, session.ID)
					if err != nil || filepath.Clean(current.RolloutPath) != filepath.Clean(session.RolloutPath) {
						return rollbackMigration(errors.New("canonical Codex route changed during migration"))
					}
				}
				result.Routed = true
				result.DryRun = false
				result.Storage = storage.CompleteAccounting(command.Context(), storageAssessment, store)
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
	command.Flags().BoolVar(&canonicalNamespace, "canonical-namespace", false, "Enroll the session at its canonical Codex path without changing SQLite routing")
	command.Flags().StringVar(&nativeRoot, "native-root", "", "Canonical native snapshot root; defaults to <codex-home>/fold-native")
	command.Flags().DurationVar(&mountWait, "mount-wait", 15*time.Second, "Maximum wait for the mounted session target")
	command.Flags().BoolVar(&apply, "apply", false, "Enroll and route the session after all gates pass")
	command.Flags().BoolVar(&compatibilityCanary, "compatibility-canary", false, "Allow an isolated canonical canary with both client checks explicitly skipped")
	addCompatibilityFlags(command, &compatibility)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func rollbackCanonicalMigration(store string, sessionID string, sourcePath string, retainedPath string, cause error) error {
	// Keep the managed route live until an exact native source is available.
	// If restoration fails, retiring state here would remove both recovery paths.
	if err := restoreCanonicalSnapshotSource(sourcePath, retainedPath); err != nil {
		return errors.Join(cause, err)
	}
	stateDirectory := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	if _, err := os.Stat(stateDirectory); errors.Is(err, os.ErrNotExist) {
		return cause
	} else if err != nil {
		return errors.Join(cause, err)
	}
	if _, err := retireManagedState(store, sessionID); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func newFSRollbackCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var mountPoint string
	var nativeRoot string
	var targetPath string
	var mountWait time.Duration
	var apply, canonicalNamespace bool
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
			currentNativeFallback := !canonicalNamespace && isGeneratedNativeFallbackPath(current.RolloutPath, store, state.SessionID)
			mount := defaultMountPoint(home, mountPoint)
			if canonicalNamespace {
				if nativeRoot == "" {
					nativeRoot = filepath.Join(home, "fold-native")
				}
				canonicalTarget, err := canonicalNativeRoute(home, nativeRoot, current.RolloutPath)
				if err != nil {
					return err
				}
				if targetPath != "" && filepath.Clean(targetPath) != filepath.Clean(canonicalTarget) {
					return errors.New("canonical rollback target must remain inside the retained native namespace")
				}
				targetPath = canonicalTarget
			} else if targetPath == "" {
				targetPath = filepath.Join(store, "fs", "fallbacks", state.SessionID, "fallback-current.jsonl")
			}
			result := FSRollbackResult{SessionID: state.SessionID, From: current.RolloutPath, Target: vfs.NativeFile{Path: filepath.Clean(targetPath)}, DryRun: !apply}
			if apply {
				if currentNativeFallback {
					storageAssessment, err := assessStoreMutation(command.Context(), store, storage.Projection{Operation: "fs-rollback"})
					if err != nil {
						return err
					}
					target, err := hashPath(current.RolloutPath)
					if err != nil {
						return err
					}
					retiredState, err := retireManagedState(store, state.SessionID)
					if err != nil {
						return err
					}
					result.Target = target
					result.RetiredState = retiredState
					result.Routed = true
					result.DryRun = false
					result.Storage = storage.CompleteAccounting(command.Context(), storageAssessment, store)
					if jsonOutput {
						return writeJSON(command, result)
					}
					_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s dry_run=%t routed=%t from=%s target=%s\n", result.SessionID, result.DryRun, result.Routed, result.From, result.Target.Path)
					return err
				}
				if canonicalNamespace {
					if err := mountHealthProbe(mount); err != nil {
						return fmt.Errorf("canonical filesystem mount is not healthy: %w", err)
					}
				}
				managed, resolver, err := openManagedSession(command.Context(), store, state)
				if err != nil {
					return err
				}
				defer resolver.Close()
				rollbackLease, err := managed.OpenWriter()
				if errors.Is(err, vfs.ErrWriterBusy) {
					return errors.New("cannot rollback while the session has an active writer")
				}
				if err != nil {
					return err
				}
				defer rollbackLease.Close()
				visible, err := managed.VisibleInfo()
				if err != nil {
					return err
				}
				reclaimableBytes := int64(0)
				if info, err := os.Stat(targetPath); err == nil && info.Mode().IsRegular() {
					reclaimableBytes = info.Size()
				} else if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				storageAssessment, err := assessStoreMutation(command.Context(), store, storage.Projection{
					Operation: "fs-rollback", AdditionalPersistentBytes: visible.Size, TemporaryBytes: visible.Size,
					TemporaryPersistentOverlapBytes: visible.Size, ReclaimableBytes: reclaimableBytes,
				})
				if err != nil {
					return err
				}
				target, err := managed.MaterializeCurrent(command.Context(), filepath.Clean(targetPath), true)
				if err != nil {
					return err
				}
				if canonicalNamespace {
					mountedTarget, err := canonicalMountRoute(home, mount, current.RolloutPath)
					if err != nil {
						return err
					}
					canonicalRoute, err := canonicalNamespaceRoute(home, mount, current.RolloutPath)
					if err != nil {
						return err
					}
					retirement, err := createRetirementRequest(store, state.SessionID, managed.State().Generation, canonicalRoute, target)
					if err != nil {
						return err
					}
					recoveryWait := mountWait
					if recoveryWait < 15*time.Second {
						recoveryWait = 15 * time.Second
					}
					restoreManagedRoute := func(cause error, retiredState string, retiredSnapshot string) error {
						var restoreErrors []error
						if retiredSnapshot != "" {
							if err := restoreCanonicalNativeSnapshot(state.NativeSnapshot.Path, retiredSnapshot); err != nil {
								restoreErrors = append(restoreErrors, err)
							}
						}
						var restored vfs.SessionState
						if retiredState == "" {
							directory := filepath.Join(store, "fs", "sessions", state.SessionID)
							if err := clearRetirementControl(directory); err != nil {
								restoreErrors = append(restoreErrors, err)
							} else {
								restored, err = vfs.RepublishSessionState(filepath.Join(directory, "state.json"))
								if err != nil {
									restoreErrors = append(restoreErrors, err)
								}
							}
						} else if err := clearRetirementControl(retiredState); err != nil {
							restoreErrors = append(restoreErrors, err)
						} else if err := restoreManagedState(store, state.SessionID, retiredState); err != nil {
							restoreErrors = append(restoreErrors, err)
						} else {
							restored, err = managedState(store, state.SessionID)
							if err != nil {
								restoreErrors = append(restoreErrors, err)
							}
						}
						if restored.Generation != 0 {
							if err := waitForMountAcknowledgement(command.Context(), store, state.SessionID, restored.Generation, canonicalRoute, recoveryWait); err != nil {
								restoreErrors = append(restoreErrors, fmt.Errorf("wait for restored managed route: %w", err))
							} else if _, err := waitForTargetMatch(command.Context(), mountedTarget, target, recoveryWait); err != nil {
								restoreErrors = append(restoreErrors, fmt.Errorf("verify restored managed route: %w", err))
							}
						}
						return errors.Join(append([]error{cause}, restoreErrors...)...)
					}
					if err := waitForRetirementAcknowledgement(command.Context(), store, state.SessionID, retirement, mountWait); err != nil {
						return restoreManagedRoute(err, "", "")
					}
					_, err = waitForTargetMatch(command.Context(), mountedTarget, target, mountWait)
					if err != nil {
						return restoreManagedRoute(fmt.Errorf("verify canonical native rollback: %w", err), "", "")
					}
					nativeTarget, err := hashPath(target.Path)
					if err != nil || nativeTarget.Bytes != target.Bytes || nativeTarget.SHA256 != target.SHA256 {
						if err == nil {
							err = errors.New("canonical native rollback target changed before retirement")
						}
						return restoreManagedRoute(fmt.Errorf("verify canonical native rollback target: %w", err), "", "")
					}
					retiredState, err := retireManagedState(store, state.SessionID)
					if err != nil {
						return restoreManagedRoute(err, "", "")
					}
					retiredSnapshot, err := retireCanonicalNativeSnapshot(store, nativeRoot, state.SessionID, state.NativeSnapshot.Path, target.Path, retiredState)
					if err != nil {
						return restoreManagedRoute(err, retiredState, "")
					}
					if err := clearRetirementControl(retiredState); err != nil {
						return restoreManagedRoute(err, retiredState, retiredSnapshot)
					}
					result.RetiredState = retiredState
				} else {
					if _, err := codex.RouteSession(command.Context(), codex.RouteOptions{CodexHome: home, SessionID: state.SessionID, ExpectedPath: current.RolloutPath, Target: codex.RouteTarget{Path: target.Path, Bytes: target.Bytes, SHA256: target.SHA256}}); err != nil {
						return err
					}
					retiredState, err := retireManagedState(store, state.SessionID)
					if err != nil {
						return err
					}
					result.RetiredState = retiredState
				}
				result.Target = target
				result.Routed = true
				result.DryRun = false
				result.Storage = storage.CompleteAccounting(command.Context(), storageAssessment, store)
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
	command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().BoolVar(&canonicalNamespace, "canonical-namespace", false, "Restore current bytes to canonical native backing without changing SQLite routing")
	command.Flags().StringVar(&nativeRoot, "native-root", "", "Canonical native rollback root; defaults to <codex-home>/fold-native")
	command.Flags().DurationVar(&mountWait, "mount-wait", 15*time.Second, "Maximum wait for native passthrough after state retirement")
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
				visible, err := managed.VisibleInfo()
				if err != nil {
					return err
				}
				persistentBytes, err := conservativeStoredBytes(visible.Size)
				if err != nil {
					return err
				}
				if persistentBytes > math.MaxInt64-persistentBytes {
					return errors.New("compact storage byte estimate overflow")
				}
				persistentBytes *= 2
				storageAssessment, err := assessStoreMutation(command.Context(), store, storage.Projection{
					Operation: "fs-compact", AdditionalPersistentBytes: persistentBytes, TemporaryBytes: visible.Size,
				})
				if err != nil {
					return err
				}
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
					if _, err := fold.Fold(ctx, fold.Session{ID: state.SessionID, Title: currentManifest.Session.Title, CWD: currentManifest.Session.CWD, RolloutPath: current.Path, Archived: true}, options); err != nil {
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
				result.Storage = storage.CompleteAccounting(command.Context(), storageAssessment, store)
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
				recoveredState := managed.State()
				_ = resolver.Close()
				if _, err := recoverInterruptedCanonicalMigration(home, store, filepath.Join(home, "fold-native"), recoveredState); err != nil {
					return err
				}
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

func recoverInterruptedCanonicalMigration(home string, store string, nativeRoot string, state vfs.SessionState) (recovered bool, resultErr error) {
	retainedPath := filepath.Join(store, "fs", "snapshots", state.SessionID, "native.jsonl")
	if filepath.Clean(state.NativeSnapshot.Path) != filepath.Clean(retainedPath) {
		return false, nil
	}
	if _, pending, err := readRetirementRequest(store, state.SessionID); err != nil {
		return false, err
	} else if pending {
		return false, nil
	}
	guard, acquired, err := vfs.TryAcquireWriterLeaseGuard(store, state.SessionID)
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return false, err
	}
	current, err := findSession(sessions, state.SessionID)
	if err != nil {
		return false, err
	}
	sourcePath, err := canonicalNativeRoute(home, nativeRoot, current.RolloutPath)
	if err != nil {
		return false, err
	}
	source, err := hashPath(sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state.BackingPath != "" {
		return false, nil
	}
	delta, err := os.Stat(state.DeltaPath)
	if err != nil {
		return false, err
	}
	if delta.Size() != 0 {
		return false, nil
	}
	if source.Bytes != state.BaseBytes || source.SHA256 != state.BaseSHA256 || source.Bytes != state.NativeSnapshot.Bytes || source.SHA256 != state.NativeSnapshot.SHA256 {
		return false, errors.New("interrupted canonical migration source no longer matches the managed base")
	}
	retiredState, err := retireManagedState(store, state.SessionID)
	if err != nil {
		return false, err
	}
	if _, err := retireCanonicalNativeSnapshot(store, nativeRoot, state.SessionID, state.NativeSnapshot.Path, sourcePath, retiredState); err != nil {
		if restoreErr := restoreManagedState(store, state.SessionID, retiredState); restoreErr != nil {
			return false, errors.Join(err, restoreErr)
		}
		return false, err
	}
	return true, nil
}

func addCompatibilityFlags(command *cobra.Command, flags *compatibilityFlags) {
	defaults := defaultCompatibilityFlags()
	command.Flags().StringVar(&flags.contractsPath, "contracts", "", "Compatibility contract directory; defaults to <store>/compatibility")
	command.Flags().StringVar(&flags.cliPath, "cli", defaults.cliPath, "Codex CLI path, or 'none' to skip CLI evaluation")
	command.Flags().StringVar(&flags.desktopPath, "desktop-app", defaults.desktopPath, "Codex desktop application path, or 'none' to skip desktop evaluation")
}

func defaultCompatibilityFlags() compatibilityFlags {
	desktop := "none"
	if runtime.GOOS == "darwin" {
		desktop = "/Applications/ChatGPT.app"
	}
	return compatibilityFlags{cliPath: "codex", desktopPath: desktop}
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

func validateCompatibilityCanary(home string, defaultHome string, store string, canonical bool, flags compatibilityFlags) error {
	home = filepath.Clean(home)
	defaultHome = filepath.Clean(defaultHome)
	store = filepath.Clean(store)
	if !canonical {
		return errors.New("compatibility canary requires canonical namespace mode")
	}
	if home == defaultHome {
		return errors.New("compatibility canary is forbidden for the real Codex home")
	}
	relative, err := filepath.Rel(home, store)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("compatibility canary store must be inside the isolated Codex home")
	}
	if flags.cliPath != "none" || flags.desktopPath != "none" {
		return errors.New("compatibility canary requires --cli none and --desktop-app none")
	}
	return nil
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

func assessStoreMutation(ctx context.Context, store string, projection storage.Projection) (storage.Assessment, error) {
	guard, err := storage.DefaultGuard(store)
	if err != nil {
		return storage.Assessment{}, err
	}
	return guard.Check(ctx, projection)
}

func conservativeStoredBytes(rawBytes int64) (int64, error) {
	if rawBytes < 0 {
		return 0, errors.New("storage byte estimate cannot be negative")
	}
	overhead := rawBytes/16 + 1<<20
	if rawBytes > math.MaxInt64-overhead {
		return 0, errors.New("storage byte estimate overflow")
	}
	return rawBytes + overhead, nil
}

func startupStorageGC(ctx context.Context, store string) (storage.StorageGCResult, bool, error) {
	if err := requireStorageHealth(ctx, store); err != nil {
		return storage.StorageGCResult{}, false, nil
	}
	result, err := storage.Collect(ctx, storage.GCOptions{StoreDir: store, Apply: true})
	return result, true, err
}

func startStorageMaintenance(
	ctx context.Context,
	diagnostics io.Writer,
	store string,
	run func(context.Context, string) (storage.StorageGCResult, bool, error),
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer debug.FreeOSMemory()
		_, _, err := run(ctx, store)
		if err != nil && !errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintf(diagnostics, "storage maintenance failed: %v\n", err)
		}
	}()
	return done
}

func startRuntimeMemoryMaintenance(ctx context.Context, filesystem *mountfs.Filesystem) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !filesystem.IOIdleFor(3 * time.Second) {
					continue
				}
				var memory runtime.MemStats
				runtime.ReadMemStats(&memory)
				if runtimeMemoryReclaimable(memory, 64<<20) {
					debug.FreeOSMemory()
				}
			}
		}
	}()
	return done
}

func runtimeMemoryReclaimable(memory runtime.MemStats, threshold uint64) bool {
	return memory.HeapIdle > memory.HeapReleased && memory.HeapIdle-memory.HeapReleased >= threshold
}

func fsDoctor(ctx context.Context, home string, store string, mount string, definitionPath string) fsctl.DoctorReport {
	var serviceStatus service.Status
	platform, platformErr := service.CurrentPlatform()
	definition, definitionErr := resolveServiceDefinitionPath(definitionPath)
	if platformErr == nil && definitionErr == nil {
		serviceStatus, platformErr = platformServiceStatus(ctx, platform, mount, definition)
	}
	if platformErr != nil || definitionErr != nil {
		serviceStatus.DaemonError = errors.Join(platformErr, definitionErr).Error()
	}
	var storageInventory storage.Inventory
	var storageLimits storage.Limits
	var availableBytes int64
	checks := []fsctl.Check{
		{Component: fsctl.ComponentDaemon, Run: func(context.Context) error {
			if !serviceStatus.DaemonRunning {
				return errors.New(serviceStatus.DaemonError)
			}
			if !serviceStatus.Build.Healthy {
				return errors.New(serviceStatus.Build.Error)
			}
			return nil
		}},
		{Component: fsctl.ComponentMount, Run: func(context.Context) error {
			if !serviceStatus.MountHealthy {
				return errors.New(serviceStatus.MountError)
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
		{Component: fsctl.ComponentStorage, Run: func(ctx context.Context) error {
			var err error
			storageInventory, err = storage.Scan(ctx, storage.Options{StoreDir: store, AllowMetadataIssues: true})
			if err != nil {
				return err
			}
			storageLimits, err = storage.LoadLimits(store)
			if err != nil {
				return err
			}
			availableBytes, err = storage.AvailableBytes(store)
			return err
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
		fsctl.Check{Component: fsctl.ComponentClient, Run: func(ctx context.Context) error {
			result, err := evaluateCompatibility(ctx, store, defaultCompatibilityFlags())
			if err != nil {
				return err
			}
			if len(result.DetectionErrors) != 0 || !result.Evaluation.Approved {
				return errors.New("installed Codex clients are not covered by exact compatibility contracts")
			}
			return nil
		}},
	)
	report := fsctl.Doctor(ctx, checks)
	report.Storage = storageInventory
	report.StorageLimits = storageLimits
	report.AvailableBytes = availableBytes
	return report
}

func defaultMountPoint(home string, explicit string) string {
	if explicit != "" {
		return filepath.Clean(explicit)
	}
	return filepath.Join(home, "fold-fs")
}

func verifiedCapability() fsctl.Capability { return fsctl.FSEnginePreview }

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

func waitForTargetMatch(ctx context.Context, target string, expected vfs.NativeFile, timeout time.Duration) (vfs.NativeFile, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		file, err := hashPath(target)
		if err == nil && file.Bytes == expected.Bytes && file.SHA256 == expected.SHA256 {
			return file, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return vfs.NativeFile{}, err
		}
		if time.Now().After(deadline) {
			return vfs.NativeFile{}, errors.New("timed out waiting for matching mounted session")
		}
		select {
		case <-ctx.Done():
			return vfs.NativeFile{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
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
