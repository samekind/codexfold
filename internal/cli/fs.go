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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samekind/codexfold/internal/cdc"
	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/compat"
	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/fsctl"
	"github.com/samekind/codexfold/internal/fskitproto"
	"github.com/samekind/codexfold/internal/fskitstatus"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/scan"
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

type FSRetireNativeResult struct {
	SessionID            string                    `json:"session_id"`
	DryRun               bool                      `json:"dry_run"`
	AlreadyRetired       bool                      `json:"already_retired"`
	SnapshotBytes        int64                     `json:"snapshot_bytes"`
	VisibleBytes         int64                     `json:"visible_bytes"`
	VisibleSHA256        string                    `json:"visible_sha256,omitempty"`
	ProofPath            string                    `json:"proof_path,omitempty"`
	ActualReclaimedBytes int64                     `json:"actual_reclaimed_bytes"`
	StorageBefore        storage.Inventory         `json:"storage_before,omitempty"`
	StorageAfter         storage.Inventory         `json:"storage_after,omitempty"`
	Proof                vfs.NativeRetirementProof `json:"proof,omitempty"`
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
	SessionIDs         []string `json:"session_ids"`
	Recovered          int      `json:"recovered"`
	PendingRetirements int      `json:"pending_retirements"`
	DryRun             bool     `json:"dry_run"`
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
	command.AddCommand(newFSRetireNativeCommand())
	command.AddCommand(newFSRecoverCommand())
	command.AddCommand(newFSEnrollCommand())
	command.AddCommand(newFSRepairRolloutCommand())
	command.AddCommand(newFSReconcileRolloutCommand())
	command.AddCommand(newFSNamespaceCommand())
	command.AddCommand(newFSServiceCommand())
	return command
}

func newFSRetireNativeCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var apply bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "retire-native <session-id>",
		Short: "Retire a canonical native snapshot after pack-only recovery proof",
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
			result := FSRetireNativeResult{SessionID: state.SessionID, DryRun: !apply, SnapshotBytes: state.NativeSnapshot.Bytes}
			proofPath := filepath.Join(store, "fs", "sessions", state.SessionID, vfs.NativeRetirementFilename)
			if state.NativeSnapshot.Path == "" {
				proof, err := vfs.LoadNativeRetirementProof(proofPath)
				if err != nil {
					return fmt.Errorf("native snapshot is empty without a valid retirement proof: %w", err)
				}
				if err := validateRetiredNativeProofForState(store, state, proof); err != nil {
					return err
				}
				result.AlreadyRetired = true
				result.SnapshotBytes = proof.Snapshot.Bytes
				result.ProofPath = proofPath
				result.Proof = proof
				result.VisibleBytes = proof.Visible.Bytes
				result.VisibleSHA256 = proof.Visible.SHA256
				if apply {
					managed, resolver, err := openPackOnlyVerifiedManagedSession(command.Context(), store, state)
					if err != nil {
						return err
					}
					verifiedState := managed.State()
					if closeErr := resolver.Close(); closeErr != nil {
						return closeErr
					}
					if verifiedState != state {
						return errors.New("managed session changed during already-retired pack-only recovery proof")
					}
					currentState, err := managedState(store, state.SessionID)
					if err != nil {
						return err
					}
					if currentState != state {
						return errors.New("managed session changed before interrupted native retirement replay")
					}
					if err := validateRetiredNativeProofForState(store, currentState, proof); err != nil {
						return err
					}
					result.StorageBefore, err = storage.Scan(command.Context(), storage.Options{StoreDir: store, AllowMetadataIssues: true})
					if err != nil {
						return err
					}
					if err := vfs.CompleteNativeSnapshotRetirement(store, proof); err != nil {
						return err
					}
					result.StorageAfter, err = storage.Scan(command.Context(), storage.Options{StoreDir: store, AllowMetadataIssues: true})
					if err != nil {
						return err
					}
					if result.StorageBefore.TotalPhysicalBytes > result.StorageAfter.TotalPhysicalBytes {
						result.ActualReclaimedBytes = result.StorageBefore.TotalPhysicalBytes - result.StorageAfter.TotalPhysicalBytes
					}
				}
				if jsonOutput {
					return writeJSON(command, result)
				}
				_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s dry_run=%t already_retired=true reclaimed=%s proof=%s\n", result.SessionID, result.DryRun, formatBytes(result.ActualReclaimedBytes), result.ProofPath)
				return err
			}
			expectedSnapshot := filepath.Join(store, "fs", "snapshots", state.SessionID, "native.jsonl")
			if filepath.Clean(state.NativeSnapshot.Path) != filepath.Clean(expectedSnapshot) {
				return errors.New("only the managed canonical snapshot can be retired")
			}
			managed, resolver, err := openPackOnlyVerifiedManagedSession(command.Context(), store, state)
			if err != nil {
				return err
			}
			defer resolver.Close()
			writer, err := managed.OpenWriter()
			if errors.Is(err, vfs.ErrWriterBusy) {
				return errors.New("cannot retire native snapshot while the session has an active writer")
			}
			if err != nil {
				return err
			}
			defer writer.Close()
			beforeVisible, err := managed.VisibleInfo()
			if err != nil {
				return err
			}
			materializedPath := filepath.Join(store, "fs", "sessions", state.SessionID, fmt.Sprintf(".native-retirement-proof-%d.jsonl", time.Now().UnixNano()))
			visible, err := managed.MaterializeCurrent(command.Context(), materializedPath, false)
			if err != nil {
				return err
			}
			defer os.Remove(materializedPath)
			afterVisible, err := managed.VisibleInfo()
			if err != nil {
				return err
			}
			if beforeVisible != afterVisible || managed.State().Generation != state.Generation || managed.State().NativeSnapshot != state.NativeSnapshot {
				return errors.New("managed session changed during native retirement proof")
			}
			result.VisibleBytes = visible.Bytes
			result.VisibleSHA256 = visible.SHA256
			result.ProofPath = proofPath
			if apply {
				result.StorageBefore, err = storage.Scan(command.Context(), storage.Options{StoreDir: store, AllowMetadataIssues: true})
				if err != nil {
					return err
				}
				result.Proof, err = managed.RetireNativeSnapshot(state.NativeSnapshot, visible)
				if err != nil {
					return err
				}
				result.StorageAfter, err = storage.Scan(command.Context(), storage.Options{StoreDir: store, AllowMetadataIssues: true})
				if err != nil {
					return err
				}
				if result.StorageBefore.TotalPhysicalBytes > result.StorageAfter.TotalPhysicalBytes {
					result.ActualReclaimedBytes = result.StorageBefore.TotalPhysicalBytes - result.StorageAfter.TotalPhysicalBytes
				}
				result.DryRun = false
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s dry_run=%t snapshot=%s visible=%s reclaimed=%s proof=%s\n", result.SessionID, result.DryRun, formatBytes(result.SnapshotBytes), formatBytes(result.VisibleBytes), formatBytes(result.ActualReclaimedBytes), result.ProofPath)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&apply, "apply", false, "Retire the verified managed canonical snapshot")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
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
		Short: "Report installed Codex client evidence for regression diagnostics",
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
			states, _, err := vfs.DiscoverSessionStatesDetailedReadOnly(store)
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
			// The resident service log is a launchd stderr file with no time
			// information of its own. Stamp it at the writer so every existing
			// report line becomes correlatable without touching each call site.
			command.SetErr(newTimestampedWriter(command.ErrOrStderr()))
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
			if apply {
				if err := ensureFSServeStore(store); err != nil {
					return err
				}
			}
			states, _, err := vfs.DiscoverSessionStatesDetailedReadOnly(store)
			if err != nil && !(errors.Is(err, os.ErrNotExist) && !apply) {
				return err
			}
			if errors.Is(err, os.ErrNotExist) {
				states = nil
			}
			deletions, err := vfs.DiscoverSessionDeletions(store)
			if err != nil && !(errors.Is(err, os.ErrNotExist) && !apply) {
				return err
			}
			if errors.Is(err, os.ErrNotExist) {
				deletions = nil
			}
			states, _ = filterDeletedSessionMetadata(states, nil, deletedSessionIDs(deletions))
			if nativeFSKitResource == "" {
				nativeFSKitResource = filepath.Join(store, "fs", "native-fskit")
			}
			if nativeFSKitSocket == "" {
				nativeFSKitSocket = defaultNativeFSKitSocket(home, nativeFSKitResource)
			}
			if frontend == "native-fskit" {
				if err := validateNativeFSKitSocketPath(nativeFSKitSocket); err != nil {
					return err
				}
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
				states, _, err = vfs.DiscoverSessionStatesDetailedReadOnly(store)
				if err != nil {
					return err
				}
				deletions, err = vfs.DiscoverSessionDeletions(store)
				if err != nil {
					return err
				}
				states, _ = filterDeletedSessionMetadata(states, nil, deletedSessionIDs(deletions))
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
				filesystem.SetSessionDeletionPublisher(func(state vfs.SessionState, route string) error {
					_, err := vfs.PublishSessionDeletion(store, state, route)
					return err
				})
				for _, deletion := range deletions {
					if err := filesystem.HideDeletedSessionAt(deletion.SessionID, deletion.Route); err != nil {
						return err
					}
				}
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
			// Session-load incidents are the only place a client I/O error can be
			// correlated with the store transition that caused it, so they are
			// recorded for both namespace shapes.
			filesystem.SetSessionLoadRecorder(func(line string) {
				_, _ = fmt.Fprintln(command.ErrOrStderr(), line)
			})
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
			enrollmentDone := make(chan struct{})
			enrollmentStatusPaths := []string{enroll.ProgressPath(store)}
			if frontend == "native-fskit" {
				enrollmentStatusPaths = append(enrollmentStatusPaths, service.FSKitStatusPath(nativeFSKitResource, "enrollment"))
			}
			flags := enrollmentFlags{
				codexHome: home, storeDir: store, mountPoint: mount, nativeRoot: nativeRoot,
				stableFor: enrollmentStableFor, batchSize: enrollmentBatchSize,
				canonicalNamespace: canonicalNamespace, canary: enrollmentCanary,
				statusPaths: enrollmentStatusPaths,
			}
			go func() {
				defer close(enrollmentDone)
				runPeriodicEnrollment(ctx, flags, enrollmentInterval, func(result FSEnrollmentApplyResult, cycleErr error) {
					if cycleErr != nil {
						if !errors.Is(cycleErr, context.Canceled) && !errors.Is(cycleErr, errEnrollmentStopped) {
							_, _ = fmt.Fprintf(command.ErrOrStderr(), "enrollment cycle failed: %v\n", cycleErr)
						}
						return
					}
					if len(result.Plan.Selected) == 0 && result.Apply.Applied == 0 && result.Maintenance.NativeCandidates == 0 {
						return
					}
					_, _ = fmt.Fprintf(command.ErrOrStderr(), "enrollment cycle sessions=%d selected=%d applied=%d native_retired=%d native_deferred=%d gc_removed=%d\n", len(result.Plan.Decisions), len(result.Plan.Selected), result.Apply.Applied, result.Maintenance.NativeRetired, result.Maintenance.NativeDeferred, result.Maintenance.StorageGC.RemovedCount)
				})
			}()
			known := make(map[string]uint64)
			knownRoutes := make(map[string]string)
			knownPacks := make(map[string]string)
			lastStateIssueSignature := ""
			lastReloadError := ""
			lastMissingKnownSignature := ""
			var managedReportMu sync.Mutex
			reportManagedReloadSafe := func(err error) {
				managedReportMu.Lock()
				defer managedReportMu.Unlock()
				reportManagedReload(command.ErrOrStderr(), err, &lastReloadError)
			}
			managedStatusPath := ""
			if frontend == "native-fskit" {
				managedStatusPath = service.FSKitStatusPath(nativeFSKitResource, "managed")
			}
			managedStatus := newManagedStatusReporterForStore(managedStatusPath, store, mount, nativeFSKitResource, func(err error) {
				_, _ = fmt.Fprintf(command.ErrOrStderr(), "write managed session status: %v\n", err)
			})
			defer func() { _ = managedStatus.Close(500 * time.Millisecond) }()
			var loadMu sync.Mutex
			var managedObservationSequence uint64
			nextManagedObservationSequence := func() uint64 {
				managedObservationSequence++
				return managedObservationSequence
			}
			openState := func(state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
				managed, resolver, err := openManagedSession(ctx, store, state)
				if err != nil {
					return nil, nil, err
				}
				return managed, resolver, nil
			}
			recordManagedLoaderFailure := func(sessionID string, started time.Time, err error) {
				delete(known, sessionID)
				delete(knownRoutes, sessionID)
				delete(knownPacks, sessionID)
				observation := managedReloadObservation{
					Sequence:        nextManagedObservationSequence(),
					Fatal:           fmt.Errorf("open managed session %s: %w", sessionID, err),
					ManagedSessions: len(known), FailureStartedAt: started,
				}
				managedStatus.Observe(observation)
				reportManagedReloadSafe(observation.Err())
			}
			filesystem.SetOwnedSessionLoader(func(sessionID string) (*vfs.Session, io.Closer, error) {
				started := time.Now()
				loadMu.Lock()
				defer loadMu.Unlock()
				if _, err := vfs.LoadSessionDeletion(store, sessionID); err == nil {
					return nil, nil, os.ErrNotExist
				} else if !errors.Is(err, os.ErrNotExist) {
					recordManagedLoaderFailure(sessionID, started, err)
					return nil, nil, err
				}
				states, issues, err := vfs.DiscoverSessionStatesDetailedReadOnly(store)
				if err != nil {
					recordManagedLoaderFailure(sessionID, started, err)
					return nil, nil, err
				}
				for _, state := range states {
					if state.SessionID == sessionID {
						managed, resolver, err := openManagedSessionDeferred(ctx, store, state)
						if err == nil {
							known[state.SessionID] = state.Generation
							knownPacks[state.SessionID] = resolver.Generation()
						} else {
							recordManagedLoaderFailure(sessionID, started, err)
						}
						return managed, resolver, err
					}
				}
				for _, issue := range issues {
					if issue.SessionID == sessionID {
						err := fmt.Errorf("managed session %s is retained while its state is unavailable: %w", sessionID, issue.Err)
						recordManagedLoaderFailure(sessionID, started, err)
						return nil, nil, err
					}
				}
				if _, previouslyManaged := known[sessionID]; previouslyManaged {
					recordManagedLoaderFailure(sessionID, started, os.ErrNotExist)
				}
				return nil, nil, os.ErrNotExist
			})
			reloadLocked := func() managedReloadObservation {
				observation := managedReloadObservation{
					Sequence: nextManagedObservationSequence(), FailureStartedAt: time.Now(),
				}
				observation.ManagedSessions = len(known)

				initialStates, initialIssues, err := vfs.DiscoverSessionStatesDetailedReadOnly(store)
				if err != nil {
					observation.Fatal = err
					return observation
				}
				deletions, err := vfs.DiscoverSessionDeletions(store)
				if err != nil {
					observation.Fatal = err
					return observation
				}
				initialStates, initialIssues = filterDeletedSessionMetadata(initialStates, initialIssues, deletedSessionIDs(deletions))
				expectedManaged := retainedManagedSessionGenerations(known, initialStates, initialIssues)
				registryRemovals := make(map[string]struct{})
				registryMissing := false
				if canonicalNamespace {
					registry, registryErr := LoadManagedSessionRegistry(store)
					switch {
					case registryErr == nil:
						mergeManagedSessionRegistryGenerations(expectedManaged, registry)
					case errors.Is(registryErr, os.ErrNotExist):
						registryMissing = true
					default:
						observation.Fatal = fmt.Errorf("load durable managed session registry: %w", registryErr)
						return observation
					}
				}
				observation.StateIssues = initialIssues
				observation.ManagedSessions = len(expectedManaged)
				var codexSessions []codex.Session
				codexSnapshotLoaded := false
				if len(deletions) != 0 || canonicalNamespace {
					codexSessions, err = codex.LoadSessions(home)
					if err != nil {
						observation.Fatal = err
						return observation
					}
					codexSnapshotLoaded = true
				}
				if canonicalNamespace && registryMissing {
					bootstrapEntries, bootstrapErr := managedSessionRegistryBootstrapEntries(store, expectedManaged)
					if bootstrapErr != nil {
						observation.Fatal = bootstrapErr
						return observation
					}
					registry, bootstrapErr := WriteManagedSessionRegistry(
						store,
						bootstrapEntries,
						ManagedSessionRegistryWriteOptions{Bootstrap: true},
					)
					if bootstrapErr != nil {
						observation.Fatal = fmt.Errorf("bootstrap durable managed session registry: %w", bootstrapErr)
						return observation
					}
					mergeManagedSessionRegistryGenerations(expectedManaged, registry)
					registryMissing = false
				}
				deleted, err := reconcileManagedSessionDeletionSet(store, filesystem, canonicalNamespace, deletions, known, knownRoutes, knownPacks)
				if err != nil {
					observation.Fatal = err
					return observation
				}
				for sessionID := range deleted {
					delete(expectedManaged, sessionID)
					registryRemovals[sessionID] = struct{}{}
				}
				states, issues, err := vfs.DiscoverSessionStatesDetailedReadOnly(store)
				if err != nil {
					observation.Fatal = err
					return observation
				}
				states, issues = filterDeletedSessionMetadata(states, issues, deleted)
				if canonicalNamespace {
					retirementResult, retirementErr := recoverCanonicalRetirementsWithOwnerHandoff(
						ctx, home, store, mount, nativeRoot,
						func(sessionID string, expectedRoute string, commit func() error) error {
							return detachCompletedManagedSession(filesystem, sessionID, expectedRoute, known, knownRoutes, knownPacks, commit)
						},
					)
					if retirementErr != nil {
						observation.Fatal = errors.Join(observation.Fatal, retirementErr)
					}
					if retirementResult.Deferred != 0 {
						observation.Fatal = errors.Join(observation.Fatal, fmt.Errorf(
							"%d canonical retirement recovery operation(s) are waiting for active leases or locks",
							retirementResult.Deferred,
						))
					}
					for _, sessionID := range retirementResult.CompletedSessionIDs {
						delete(expectedManaged, sessionID)
						registryRemovals[sessionID] = struct{}{}
					}
					if retirementErr != nil || retirementResult.Completed != 0 || retirementResult.Restored != 0 {
						states, issues, err = vfs.DiscoverSessionStatesDetailedReadOnly(store)
						if err != nil {
							observation.Fatal = errors.Join(observation.Fatal, err)
							return observation
						}
						states, issues = filterDeletedSessionMetadata(states, issues, deleted)
					}
				}
				if canonicalNamespace && !codexSnapshotLoaded && (len(states) != 0 || len(issues) != 0) {
					codexSessions, err = codex.LoadSessions(home)
					if err != nil {
						observation.Fatal = err
						return observation
					}
					codexSnapshotLoaded = true
				}
				if canonicalNamespace && len(issues) != 0 {
					if !codexSnapshotLoaded {
						observation.Fatal = errors.Join(observation.Fatal, errors.New("managed state recovery requires a complete Codex metadata snapshot"))
						return observation
					}
					recoveredState, recoveryErr := recoverManagedSessionStateIssues(store, codexSessions, issues, deleted)
					if recoveryErr != nil {
						observation.Fatal = errors.Join(observation.Fatal, recoveryErr)
					}
					if recoveredState {
						states, issues, err = vfs.DiscoverSessionStatesDetailedReadOnly(store)
						if err != nil {
							observation.Fatal = errors.Join(observation.Fatal, err)
							return observation
						}
						states, issues = filterDeletedSessionMetadata(states, issues, deleted)
					}
				}
				if canonicalNamespace && len(states) != 0 {
					if !codexSnapshotLoaded {
						observation.Fatal = errors.New("interrupted canonical migration recovery requires a complete Codex metadata snapshot")
						return observation
					}
					retiredSessions, recoveryErr := recoverInterruptedCanonicalMigrationsWithOwnerHandoff(
						home, store, nativeRoot, states, codexSessions,
						func(sessionID string, expectedRoute string, commit func() error) error {
							return detachCompletedManagedSession(filesystem, sessionID, expectedRoute, known, knownRoutes, knownPacks, commit)
						},
					)
					if recoveryErr != nil {
						observation.Fatal = errors.Join(observation.Fatal, recoveryErr)
					}
					for _, sessionID := range retiredSessions {
						delete(expectedManaged, sessionID)
						registryRemovals[sessionID] = struct{}{}
					}
					if len(retiredSessions) != 0 {
						states, issues, err = vfs.DiscoverSessionStatesDetailedReadOnly(store)
						if err != nil {
							observation.Fatal = errors.Join(observation.Fatal, err)
							return observation
						}
						states, issues = filterDeletedSessionMetadata(states, issues, deleted)
					}
				}
				missingManifests, err := missingManagedManifestIDs(store, states)
				if err != nil {
					observation.Fatal = err
					return observation
				}
				if len(missingManifests) != 0 {
					if !codexSnapshotLoaded {
						codexSessions, err = codex.LoadSessions(home)
						if err != nil {
							observation.Fatal = errors.Join(observation.Fatal, fmt.Errorf("automatic managed manifest recovery requires a complete Codex metadata snapshot: %w", err))
							return observation
						}
						codexSnapshotLoaded = true
					}
					if _, err := pack.RepairCurrentManifests(store); err != nil {
						observation.Fatal = fmt.Errorf("repair missing managed manifests: %w", err)
						return observation
					}
					states, issues, err = vfs.DiscoverSessionStatesDetailedReadOnly(store)
					if err != nil {
						observation.Fatal = err
						return observation
					}
					states, issues = filterDeletedSessionMetadata(states, issues, deleted)
					stillMissing, checkErr := missingManagedManifestIDs(store, states)
					if checkErr != nil {
						observation.Fatal = checkErr
						return observation
					}
					if len(stillMissing) != 0 {
						observation.Fatal = fmt.Errorf("managed manifests remain unavailable after recovery: %s", strings.Join(stillMissing, ","))
						return observation
					}
				}
				observation.StateIssues = issues
				reportManagedStateIssues(command.ErrOrStderr(), issues, &lastStateIssueSignature)
				if canonicalNamespace {
					// Recovery and repair can outlive a SQLite route change. Refresh at
					// the final publication boundary so stale metadata never authorizes a
					// Move, Upsert, retirement acknowledgement, or owner switch.
					ids := make([]string, 0, len(states))
					for _, state := range states {
						ids = append(ids, state.SessionID)
					}
					codexSessions, err = codex.LoadSessionsByID(home, ids)
					if err != nil {
						observation.Fatal = errors.Join(observation.Fatal, fmt.Errorf("refresh Codex metadata before managed route publication: %w", err))
						return observation
					}
					codexSnapshotLoaded = true
				}
				currentPack, err := currentPackForStates(store, states)
				if err != nil {
					observation.Fatal = err
					return observation
				}
				routes := make(map[string]string)
				if canonicalNamespace {
					routes, err = canonicalSessionRoutes(home, mount, store, states, codexSessions)
					if err != nil {
						observation.Fatal = err
						return observation
					}
				}
				missingRoutes := make([]string, 0)
				for _, state := range states {
					if canonicalNamespace {
						route, exists := routes[state.SessionID]
						if !exists {
							missingRoutes = append(missingRoutes, state.SessionID)
						}
						handled, err := syncCanonicalRetirement(ctx, store, home, nativeRoot, filesystem, state, route, exists, known, knownRoutes, knownPacks, currentPack, openState)
						if err != nil {
							observation.Fatal = err
							return observation
						}
						if handled {
							continue
						}
						if !exists {
							// Codex metadata or a route can disappear transiently. Keep the
							// last-known-good owner until an explicit retirement/tombstone
							// authorizes removal.
							continue
						}
						generation, generationKnown := known[state.SessionID]
						if generationKnown && generation == state.Generation && knownPacks[state.SessionID] == currentPack {
							if knownRoutes[state.SessionID] == route {
								continue
							}
							if err := filesystem.MoveSessionAt(state.SessionID, route); err != nil {
								observation.Fatal = err
								return observation
							}
							if err := writeMountAcknowledgement(store, state.SessionID, state.Generation, route); err != nil {
								observation.Fatal = err
								return observation
							}
							knownRoutes[state.SessionID] = route
							continue
						}
						if err := upsertCanonicalManagedState(store, filesystem, state, route, known, knownRoutes, knownPacks, openState); err != nil {
							observation.Fatal = err
							return observation
						}
						continue
					}
					if known[state.SessionID] == state.Generation && knownPacks[state.SessionID] == currentPack {
						continue
					}
					managed, resolver, err := openState(state)
					if err != nil {
						observation.Fatal = err
						return observation
					}
					if err := filesystem.UpsertSessionOwned(state.SessionID, managed, resolver); err != nil {
						observation.Fatal = err
						return observation
					}
					known[state.SessionID] = managed.State().Generation
					knownPacks[state.SessionID] = resolver.Generation()
				}
				if canonicalNamespace {
					finalSnapshot, registryErr := publishManagedSessionRegistryAtFinalFence(store, known, registryRemovals)
					if registryErr != nil {
						observation.Fatal = fmt.Errorf("publish durable managed session registry: %w", registryErr)
						return observation
					}
					expectedManaged = finalSnapshot.Retained
					states, issues = finalSnapshot.States, finalSnapshot.Issues
					observation.StateIssues = issues
					reportManagedStateIssues(command.ErrOrStderr(), issues, &lastStateIssueSignature)
				} else {
					expectedManaged = retainedManagedSessionGenerations(known, states, issues)
				}
				seen := managedSessionIDsRetained(states, issues)
				observation.MissingState = missingManagedSessionIDs(expectedManaged, seen)
				observation.MissingRoute = missingRoutes
				observation.ManagedSessions = retainedManagedSessionCount(expectedManaged, seen)
				reportMissingKnownSessions(command.ErrOrStderr(), expectedManaged, seen, &lastMissingKnownSignature)
				return observation
			}
			load := func() error {
				loadMu.Lock()
				defer loadMu.Unlock()
				observation := reloadLocked()
				managedStatus.Observe(observation)
				return observation.Err()
			}
			reportManagedReloadSafe(load())
			storageMaintenanceDone := startStorageMaintenance(ctx, command.ErrOrStderr(), store, startupStorageGC)
			var storageStatusDone <-chan struct{}
			var activityCounter *mountfs.IOActivityCounter
			if frontend == "native-fskit" {
				storageStatusDone = startStorageStatusReporter(
					ctx,
					command.ErrOrStderr(),
					store,
					service.FSKitStatusPath(nativeFSKitResource, "storage"),
					time.Minute,
					storageMaintenanceDone,
					storage.Scan,
					fskitstatus.Write,
				)
				activityCounter = &mountfs.IOActivityCounter{}
			}
			runtimeMemoryMaintenanceDone := startRuntimeMemoryMaintenance(ctx, filesystem)
			watcherDone := make(chan struct{})
			go func() {
				defer close(watcherDone)
				runManagedReloadLoop(ctx, time.Second, 30*time.Second, load, func(err error) {
					reportManagedReloadSafe(err)
				})
			}()
			var mountErr error
			if frontend == "native-fskit" {
				mountErr = mountfs.ServeNativeFSKit(ctx, filesystem, mountfs.NativeFSKitServerOptions{
					SocketPath: nativeFSKitSocket, ResourcePath: nativeFSKitResource,
					StatusPath: service.FSKitStatusPath(nativeFSKitResource, "daemon"), MountPoint: mount,
					Recorder:                   operationRecorder,
					Activity:                   activityCounter,
					PrewarmSharedMemoryWindows: 4,
				})
			} else {
				mountErr = mountfs.Mount(ctx, mountfs.HostOptions{MountPoint: mount, Filesystem: filesystem, Foreground: foreground, OperationRecorder: operationRecorder})
			}
			cancel()
			<-watcherDone
			<-storageMaintenanceDone
			if storageStatusDone != nil {
				<-storageStatusDone
			}
			<-runtimeMemoryMaintenanceDone
			<-enrollmentDone
			var nativeWatcherErr error
			if nativeWatcherDone != nil {
				nativeWatcherErr = <-nativeWatcherDone
				if errors.Is(nativeWatcherErr, context.Canceled) {
					nativeWatcherErr = nil
				}
			}
			sessionCloseErr := filesystem.CloseSessions()
			return errors.Join(mountErr, nativeWatcherErr, sessionCloseErr)
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
	command.Flags().BoolVar(&enrollmentCanary, "enrollment-canary", false, "Enable additional isolated-home constraints for periodic validation")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output for dry-run")
	return command
}

func ensureFSServeStore(store string) error {
	if !filepath.IsAbs(store) {
		return errors.New("filesystem service store must be absolute")
	}
	if err := os.MkdirAll(store, 0o700); err != nil {
		return fmt.Errorf("create filesystem service store: %w", err)
	}
	info, err := os.Lstat(store)
	if err != nil {
		return fmt.Errorf("inspect filesystem service store: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("filesystem service store must be a real directory")
	}
	return nil
}

func managedSessionIDsRetained(states []vfs.SessionState, issues []vfs.SessionStateIssue) map[string]struct{} {
	retained := make(map[string]struct{}, len(states)+len(issues))
	for _, state := range states {
		retained[state.SessionID] = struct{}{}
	}
	for _, issue := range issues {
		if issue.SessionID != "" {
			retained[issue.SessionID] = struct{}{}
		}
	}
	return retained
}

func retainedManagedSessionGenerations(known map[string]uint64, states []vfs.SessionState, issues []vfs.SessionStateIssue) map[string]uint64 {
	retained := make(map[string]uint64, len(known)+len(states)+len(issues))
	for sessionID, generation := range known {
		retained[sessionID] = generation
	}
	for _, state := range states {
		if state.Generation > retained[state.SessionID] {
			retained[state.SessionID] = state.Generation
		}
	}
	for _, issue := range issues {
		if issue.SessionID != "" {
			if _, exists := retained[issue.SessionID]; !exists {
				retained[issue.SessionID] = 0
			}
		}
	}
	return retained
}

func mergeManagedSessionRegistryGenerations(retained map[string]uint64, registry ManagedSessionRegistry) {
	for _, entry := range registry.Entries {
		if entry.Generation > retained[entry.ID] {
			retained[entry.ID] = entry.Generation
		}
	}
}

func managedSessionRegistryEntriesFromGenerations(retained map[string]uint64) []ManagedSessionRegistryEntry {
	entries := make([]ManagedSessionRegistryEntry, 0, len(retained))
	for sessionID, generation := range retained {
		if generation == 0 {
			continue
		}
		entries = append(entries, ManagedSessionRegistryEntry{ID: sessionID, Generation: generation})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

func deletedSessionIDs(deletions []vfs.SessionDeletion) map[string]struct{} {
	deleted := make(map[string]struct{}, len(deletions))
	for _, deletion := range deletions {
		deleted[deletion.SessionID] = struct{}{}
	}
	return deleted
}

func filterDeletedSessionMetadata(states []vfs.SessionState, issues []vfs.SessionStateIssue, deleted map[string]struct{}) ([]vfs.SessionState, []vfs.SessionStateIssue) {
	if len(deleted) == 0 {
		return states, issues
	}
	activeStates := make([]vfs.SessionState, 0, len(states))
	for _, state := range states {
		if _, exists := deleted[state.SessionID]; !exists {
			activeStates = append(activeStates, state)
		}
	}
	activeIssues := make([]vfs.SessionStateIssue, 0, len(issues))
	for _, issue := range issues {
		if _, exists := deleted[issue.SessionID]; !exists {
			activeIssues = append(activeIssues, issue)
		}
	}
	return activeStates, activeIssues
}

func reconcileManagedSessionDeletions(
	store string,
	filesystem *mountfs.Filesystem,
	canonicalNamespace bool,
	known map[string]uint64,
	knownRoutes map[string]string,
	knownPacks map[string]string,
) (map[string]struct{}, error) {
	deletions, err := vfs.DiscoverSessionDeletions(store)
	if err != nil {
		return nil, err
	}
	return reconcileManagedSessionDeletionSet(store, filesystem, canonicalNamespace, deletions, known, knownRoutes, knownPacks)
}

func reconcileManagedSessionDeletionSet(
	store string,
	filesystem *mountfs.Filesystem,
	canonicalNamespace bool,
	deletions []vfs.SessionDeletion,
	known map[string]uint64,
	knownRoutes map[string]string,
	knownPacks map[string]string,
) (map[string]struct{}, error) {
	deleted := deletedSessionIDs(deletions)
	var replayErrors []error
	for _, deletion := range deletions {
		if err := filesystem.RemoveSession(deletion.SessionID); err != nil && !errors.Is(err, os.ErrNotExist) {
			replayErrors = append(replayErrors, fmt.Errorf("detach deleted managed session %s: %w", deletion.SessionID, err))
		}
		delete(known, deletion.SessionID)
		delete(knownRoutes, deletion.SessionID)
		delete(knownPacks, deletion.SessionID)
		if canonicalNamespace {
			if err := filesystem.HideDeletedSessionAt(deletion.SessionID, deletion.Route); err != nil {
				replayErrors = append(replayErrors, err)
				continue
			}
		}
		if _, err := vfs.AdvanceSessionDeletion(store, deletion); err != nil && !errors.Is(err, vfs.ErrSessionDeletionBusy) {
			replayErrors = append(replayErrors, fmt.Errorf("replay managed session deletion %s: %w", deletion.SessionID, err))
		}
	}
	return deleted, errors.Join(replayErrors...)
}

func reportManagedStateIssues(writer io.Writer, issues []vfs.SessionStateIssue, previous *string) {
	var builder strings.Builder
	for _, issue := range issues {
		fmt.Fprintf(&builder, "%s\x00%s\x00%s\x00%s\n", issue.SessionID, issue.Kind, issue.Path, issue.Err)
	}
	signature := builder.String()
	if signature == *previous {
		return
	}
	if signature == "" {
		if *previous != "" {
			_, _ = fmt.Fprintln(writer, "managed session state issues cleared")
		}
		*previous = signature
		return
	}
	for _, issue := range issues {
		_, _ = fmt.Fprintf(writer, "managed session retained session=%s issue=%s path=%s error=%v\n", issue.SessionID, issue.Kind, issue.Path, issue.Err)
	}
	*previous = signature
}

func reportMissingKnownSessions(writer io.Writer, known map[string]uint64, seen map[string]struct{}, previous *string) {
	missing := make([]string, 0)
	for sessionID := range known {
		if _, exists := seen[sessionID]; !exists {
			missing = append(missing, sessionID)
		}
	}
	sort.Strings(missing)
	signature := strings.Join(missing, ",")
	if signature == *previous {
		return
	}
	if signature == "" {
		if *previous != "" {
			_, _ = fmt.Fprintln(writer, "managed session metadata presence recovered")
		}
	} else {
		_, _ = fmt.Fprintf(writer, "managed sessions retained without current metadata sessions=%s\n", signature)
	}
	*previous = signature
}

func reportManagedReload(writer io.Writer, err error, previous *string) {
	current := ""
	if err != nil {
		current = err.Error()
	}
	if current == *previous {
		return
	}
	if current == "" {
		if *previous != "" {
			_, _ = fmt.Fprintln(writer, "managed session reload recovered")
		}
	} else {
		_, _ = fmt.Fprintf(writer, "managed session reload deferred; last-known-good sessions remain mounted: %v\n", err)
	}
	*previous = current
}

type managedReloadDelayState struct {
	minimumDelay     time.Duration
	maximumDelay     time.Duration
	recoveryDeadline time.Duration
	failureStartedAt time.Time
	backoffDelay     time.Duration
}

func newManagedReloadDelayState(minimumDelay time.Duration, maximumDelay time.Duration, recoveryDeadline time.Duration) managedReloadDelayState {
	if minimumDelay <= 0 {
		minimumDelay = time.Second
	}
	if maximumDelay < minimumDelay {
		maximumDelay = minimumDelay
	}
	if recoveryDeadline < 0 {
		recoveryDeadline = 0
	}
	return managedReloadDelayState{
		minimumDelay: minimumDelay, maximumDelay: maximumDelay,
		recoveryDeadline: recoveryDeadline, backoffDelay: minimumDelay,
	}
}

func (state *managedReloadDelayState) next(err error, observedAt time.Time) time.Duration {
	if err == nil {
		state.failureStartedAt = time.Time{}
		state.backoffDelay = state.minimumDelay
		return state.minimumDelay
	}
	if state.failureStartedAt.IsZero() {
		state.failureStartedAt = observedAt
		state.backoffDelay = state.minimumDelay
		return state.minimumDelay
	}
	if observedAt.Sub(state.failureStartedAt) < state.recoveryDeadline {
		state.backoffDelay = state.minimumDelay
		return state.minimumDelay
	}
	if state.backoffDelay > state.maximumDelay/2 {
		state.backoffDelay = state.maximumDelay
	} else {
		state.backoffDelay *= 2
		if state.backoffDelay > state.maximumDelay {
			state.backoffDelay = state.maximumDelay
		}
	}
	return state.backoffDelay
}

func runManagedReloadLoop(ctx context.Context, minimumDelay time.Duration, maximumDelay time.Duration, load func() error, report func(error)) {
	delayState := newManagedReloadDelayState(minimumDelay, maximumDelay, managedRecoveryDeadline)
	delay := delayState.minimumDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		err := load()
		report(err)
		delay = delayState.next(err, time.Now())
	}
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

func validateNativeFSKitSocketPath(path string) error {
	if len(path) >= 104 {
		return errors.New("native FSKit Unix socket path exceeds the macOS limit; choose a shorter resource path")
	}
	return nil
}

func syncCanonicalRetirement(
	ctx context.Context,
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
	return syncCanonicalRetirementWithFinalFenceHook(
		ctx, store, home, nativeRoot, filesystem, state, route, routeExists,
		known, knownRoutes, knownPacks, currentPack, openState, nil,
	)
}

func syncCanonicalRetirementWithFinalFenceHook(
	ctx context.Context,
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
	beforeFinalFence func() error,
) (bool, error) {
	retirement, retiring, err := readRetirementRequest(store, state.SessionID)
	if err != nil {
		return false, err
	}
	if !retiring {
		return false, removeIfExists(filepath.Join(store, "fs", "sessions", state.SessionID, retirementAcknowledgementFilename))
	}
	deletionLock, err := storage.AcquireOperationLock(store, "session-deletions")
	if errors.Is(err, storage.ErrOperationLockHeld) {
		return true, fmt.Errorf("%w: retirement cutover is waiting for explicit deletion reconciliation", storage.ErrOperationLockHeld)
	}
	if err != nil {
		return true, err
	}
	defer deletionLock.Close()
	currentRetirement, stillRetiring, err := readRetirementRequest(store, state.SessionID)
	if err != nil {
		return true, err
	}
	if !stillRetiring || currentRetirement != retirement {
		return true, errors.New("retirement request changed before cutover serialization")
	}
	if _, err := vfs.LoadSessionDeletion(store, state.SessionID); err == nil {
		// Explicit deletion is the higher authority. Leave the retirement request
		// untouched so deletion replay can quarantine the exact managed evidence.
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	activeDirectory := filepath.Join(filepath.Clean(store), "fs", "sessions", state.SessionID)
	acknowledgement, err := validateRetirementAcknowledgement(activeDirectory, retirement)
	if err != nil {
		return true, err
	}
	if acknowledgement.Acknowledged || acknowledgement.Rejected {
		return true, nil
	}
	reject := func(message string) (bool, error) {
		return true, ensureRetirementRejectionAt(activeDirectory, retirement, message)
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
	guard, acquired, err := vfs.TryAcquireWriterLeaseGuard(store, state.SessionID)
	if err != nil {
		return true, err
	}
	if !acquired {
		return true, errors.New("retirement cutover is waiting for the managed writer lease")
	}
	defer guard.Close()
	recovered, err := vfs.RecoverSessionJournalWithWriterLease(ctx, filepath.Join(activeDirectory, "state.json"))
	if err != nil {
		return true, fmt.Errorf("recover managed journal before retirement cutover: %w", err)
	}
	if recovered.SessionID != state.SessionID || recovered.Generation != retirement.Generation {
		return reject("managed session advanced after retirement request publication")
	}
	binding, err := loadCurrentRetirementStateBinding(activeDirectory, state.SessionID, retirement.Generation)
	if err != nil || binding.StateSHA256 != retirement.StateSHA256 || binding.CheckpointSequence != retirement.CheckpointSequence || binding.CheckpointSHA256 != retirement.CheckpointSHA256 {
		return reject("managed session advanced after retirement request publication")
	}
	nativeTargetPath, err := canonicalNativeRoute(home, nativeRoot, filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(route, "/"))))
	if err != nil {
		return true, err
	}
	nativeTarget, targetErr := hashStableCanonicalNativeFile(nativeRoot, nativeTargetPath)
	if targetErr != nil || nativeTarget.Bytes != retirement.Bytes || nativeTarget.SHA256 != retirement.SHA256 {
		return reject("native rollback target is unavailable or changed")
	}
	if err := filesystem.RemoveSessionAtWithCommit(state.SessionID, route, func() error {
		if _, err := vfs.LoadSessionDeletion(store, state.SessionID); err == nil {
			return errors.New("explicit deletion tombstone supersedes retirement cutover")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		binding, err := loadCurrentRetirementStateBinding(filepath.Join(filepath.Clean(store), "fs", "sessions", state.SessionID), state.SessionID, retirement.Generation)
		if err != nil {
			return publishRetirementCutoverRejectionAt(activeDirectory, retirement, "managed checkpoint changed before retirement cutover")
		}
		if binding.StateSHA256 != retirement.StateSHA256 || binding.CheckpointSequence != retirement.CheckpointSequence || binding.CheckpointSHA256 != retirement.CheckpointSHA256 {
			return publishRetirementCutoverRejectionAt(activeDirectory, retirement, "managed session advanced after retirement request publication")
		}
		if beforeFinalFence != nil {
			if err := beforeFinalFence(); err != nil {
				return err
			}
		}
		if !canonicalNativeFileStillMatches(nativeRoot, nativeTargetPath, nativeTarget) {
			return publishRetirementCutoverRejectionAt(activeDirectory, retirement, "native rollback target changed at the retirement cutover boundary")
		}
		return writeRetirementAcknowledgement(store, state.SessionID, retirement)
	}); err != nil {
		if errors.Is(err, errRetirementCutoverRejected) {
			return true, nil
		}
		if errors.Is(err, mountfs.ErrManagedSessionRouteChanged) {
			return reject("managed session route changed before owner retirement")
		}
		if errors.Is(err, mountfs.ErrManagedSessionDeletionInProgress) {
			return true, nil
		}
		return true, err
	}
	delete(known, state.SessionID)
	delete(knownRoutes, state.SessionID)
	delete(knownPacks, state.SessionID)
	return true, nil
}

func createCanonicalRetirementRequestWithDeletionFence(store string, sessionID string, generation uint64, route string, target vfs.NativeFile) (request retirementControl, resultErr error) {
	deletionLock, err := storage.AcquireOperationLock(store, "session-deletions")
	if err != nil {
		return retirementControl{}, fmt.Errorf("serialize retirement request with explicit deletion: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, deletionLock.Close()) }()
	if _, err := vfs.LoadSessionDeletion(store, sessionID); err == nil {
		return retirementControl{}, errors.New("explicit session deletion supersedes canonical retirement")
	} else if !errors.Is(err, os.ErrNotExist) {
		return retirementControl{}, err
	}
	return createRetirementRequest(store, sessionID, generation, route, target)
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
				writerActive, err := filesystemMigrationWriterProbe(command.Context(), session, sourcePath)
				if err != nil {
					return fmt.Errorf("probe native session writer: %w", err)
				}
				if writerActive {
					return fmt.Errorf("session %s has an active native writer; close Codex before migration", session.ID)
				}
				currentNative, err := hashPath(sourcePath)
				if err != nil {
					return fmt.Errorf("recheck native session after writer probe: %w", err)
				}
				if currentNative.Bytes != native.Bytes || currentNative.SHA256 != native.SHA256 {
					return errors.New("native session changed during writer probe; migration was not applied")
				}
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
					return rollbackCanonicalMigration(canonicalSource, native.Path, cause)
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
					writerActive, err := filesystemMigrationWriterProbe(command.Context(), session, canonicalSource, native.Path)
					if err != nil {
						return rollbackMigration(fmt.Errorf("recheck native session writer before canonical cutover: %w", err))
					}
					if writerActive {
						return rollbackMigration(fmt.Errorf("session %s acquired an active native writer during migration", session.ID))
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

func rollbackCanonicalMigration(sourcePath string, retainedPath string, cause error) error {
	// A standalone migration command cannot detach the daemon's live owner.
	// Preserve both native and managed evidence; an eventual switch back to
	// native must use the daemon retirement handoff.
	if err := preserveCanonicalSnapshotSource(sourcePath, retainedPath); err != nil {
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
				rollbackLeaseOpen := true
				defer func() {
					if rollbackLeaseOpen {
						_ = rollbackLease.Close()
					}
				}()
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
					canonicalRoute, err := canonicalNamespaceRoute(home, mount, current.RolloutPath)
					if err != nil {
						return err
					}
					retirement, err := createCanonicalRetirementRequestWithDeletionFence(store, state.SessionID, managed.State().Generation, canonicalRoute, target)
					if err != nil {
						return err
					}
					// The durable request binds the exact checkpoint. Release the
					// initiator lease before waiting so the daemon can acquire the same
					// writer guard and atomically detach+ack. Any intervening managed
					// write advances the checkpoint and makes the daemon reject cutover.
					if err := rollbackLease.Close(); err != nil {
						return fmt.Errorf("release rollback writer lease after durable request publication: %w", err)
					}
					rollbackLeaseOpen = false
					recoveryWait := mountWait
					if recoveryWait < 15*time.Second {
						recoveryWait = 15 * time.Second
					}
					restoreManagedRoute := func(cause error, retiredState string, retiredSnapshot string) error {
						var restoreErrors []error
						// Only a real failure may be recorded. `len(restoreErrors) == 0`
						// is what decides whether this compensation goes on to wait for
						// the restored route, and appending a nil still grows the slice:
						// closing the lock cleanly used to make that check false every
						// time, so the wait and the verification below never ran.
						recordRestoreError := func(errs ...error) {
							if joined := errors.Join(errs...); joined != nil {
								restoreErrors = append(restoreErrors, joined)
							}
						}
						deletionLock, lockErr := storage.AcquireOperationLock(store, "session-deletions")
						if lockErr != nil {
							return errors.Join(cause, fmt.Errorf("serialize retirement compensation with explicit deletion: %w", lockErr))
						}
						lockOpen := true
						closeDeletionLock := func() {
							if lockOpen {
								recordRestoreError(deletionLock.Close())
								lockOpen = false
							}
						}
						defer closeDeletionLock()
						if _, deletionErr := vfs.LoadSessionDeletion(store, state.SessionID); deletionErr == nil {
							return errors.Join(cause, errors.New("explicit session deletion superseded retirement compensation"))
						} else if !errors.Is(deletionErr, os.ErrNotExist) {
							return errors.Join(cause, deletionErr)
						}
						if retiredSnapshot != "" {
							if err := restoreCanonicalNativeSnapshot(state.NativeSnapshot.Path, retiredSnapshot); err != nil {
								restoreErrors = append(restoreErrors, err)
							}
						}
						var restored vfs.SessionState
						if retiredState == "" {
							directory := filepath.Join(store, "fs", "sessions", state.SessionID)
							_, pending, requestErr := readRetirementRequest(store, state.SessionID)
							if requestErr != nil {
								restoreErrors = append(restoreErrors, requestErr)
							} else if pending {
								restored, err = vfs.RepublishSessionState(filepath.Join(directory, "state.json"))
								if err != nil {
									restoreErrors = append(restoreErrors, err)
								} else if err := upsertManagedSessionRegistryEntryIfPresent(store, restored.SessionID, restored.Generation); err != nil {
									restoreErrors = append(restoreErrors, err)
								} else if err := clearRetirementControl(directory); err != nil {
									restoreErrors = append(restoreErrors, err)
								}
							} else {
								restored, err = managedState(store, state.SessionID)
								if err != nil {
									restoreErrors = append(restoreErrors, err)
								} else if restored.Generation <= retirement.Generation {
									restoreErrors = append(restoreErrors, errors.New("retirement request disappeared without committed or compensated state"))
								}
							}
						} else if err := restoreManagedStateWithWriterLease(store, state.SessionID, retiredState); err != nil {
							restoreErrors = append(restoreErrors, err)
						} else {
							restored, err = managedState(store, state.SessionID)
							if err != nil {
								restoreErrors = append(restoreErrors, err)
							}
						}
						var restoredVisible vfs.NativeFile
						if restored.Generation != 0 && len(restoreErrors) == 0 {
							restoredManaged, restoredResolver, openErr := openManagedSession(command.Context(), store, restored)
							if openErr != nil {
								restoreErrors = append(restoreErrors, openErr)
							} else {
								restored = restoredManaged.State()
								restoredVisible, openErr = hashManagedSessionVisible(command.Context(), restoredManaged)
								recordRestoreError(openErr, restoredResolver.Close())
								if err := upsertManagedSessionRegistryEntryIfPresent(store, restored.SessionID, restored.Generation); err != nil {
									restoreErrors = append(restoreErrors, err)
								}
							}
						}
						closeDeletionLock()
						if restored.Generation != 0 && len(restoreErrors) == 0 {
							freshSessions, freshErr := codex.LoadSessions(home)
							if freshErr != nil {
								restoreErrors = append(restoreErrors, freshErr)
								return errors.Join(append([]error{cause}, restoreErrors...)...)
							}
							freshSession, freshErr := findSession(freshSessions, state.SessionID)
							if freshErr != nil {
								restoreErrors = append(restoreErrors, freshErr)
								return errors.Join(append([]error{cause}, restoreErrors...)...)
							}
							freshRoute, freshErr := canonicalNamespaceRoute(home, mount, freshSession.RolloutPath)
							if freshErr != nil {
								restoreErrors = append(restoreErrors, freshErr)
								return errors.Join(append([]error{cause}, restoreErrors...)...)
							}
							freshMountedTarget, freshErr := canonicalMountRoute(home, mount, freshSession.RolloutPath)
							if freshErr != nil {
								restoreErrors = append(restoreErrors, freshErr)
								return errors.Join(append([]error{cause}, restoreErrors...)...)
							}
							if err := waitForMountAcknowledgement(command.Context(), store, state.SessionID, restored.Generation, freshRoute, recoveryWait); err != nil {
								restoreErrors = append(restoreErrors, fmt.Errorf("wait for restored managed route: %w", err))
							} else if _, err := waitForTargetPrefix(command.Context(), freshMountedTarget, restoredVisible, recoveryWait); err != nil {
								restoreErrors = append(restoreErrors, fmt.Errorf("verify restored managed route: %w", err))
							}
						}
						return errors.Join(append([]error{cause}, restoreErrors...)...)
					}
					if err := waitForRetirementAcknowledgement(command.Context(), store, state.SessionID, retirement, mountWait); err != nil {
						if errors.Is(err, errRetirementRejected) {
							return restoreManagedRoute(err, "", "")
						}
						return fmt.Errorf("canonical retirement cutover is pending automatic recovery: %w", err)
					}
					retiredState, err := waitForCanonicalRetirementCompletion(command.Context(), store, state.SessionID, recoveryWait)
					if err != nil {
						return fmt.Errorf("canonical retirement is committed and pending daemon completion: %w", err)
					}
					freshSessions, err := codex.LoadSessions(home)
					if err != nil {
						return fmt.Errorf("refresh Codex route after canonical retirement: %w", err)
					}
					if freshSession, findErr := findSession(freshSessions, state.SessionID); findErr == nil {
						freshMountedTarget, routeErr := canonicalMountRoute(home, mount, freshSession.RolloutPath)
						if routeErr != nil {
							return routeErr
						}
						if _, err := waitForTargetPrefix(command.Context(), freshMountedTarget, target, mountWait); err != nil {
							return fmt.Errorf("canonical retirement committed but current mounted route failed prefix verification: %w", err)
						}
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
	var recordIndexPath string
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
				var recordIndex *scan.DuplicateRecordIndex
				if recordIndexPath != "" {
					recordIndex, err = scan.OpenDuplicateRecordIndex(recordIndexPath)
					if err != nil {
						return err
					}
					defer recordIndex.Close()
				}
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
						ExistingReader: resolver,
						FieldThreshold: currentManifest.Settings.FieldThreshold, MaxJSONLineBytes: currentManifest.Settings.MaxJSONLineBytes,
						CDC: cdc.Options{MinBytes: currentManifest.Settings.CDCMinBytes, AverageBytes: currentManifest.Settings.CDCAverageBytes, MaxBytes: currentManifest.Settings.CDCMaxBytes},
					}
					if recordIndex != nil {
						options.RecordIndex = recordIndex
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
	command.Flags().StringVar(&recordIndexPath, "record-index", "", "Explicit record-layer scan index for conservative Fold V2")
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
				if _, pending, err := readRetirementRequest(store, recoveredState.SessionID); err != nil {
					return err
				} else if pending {
					result.PendingRetirements++
				}
				result.Recovered++
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t selected=%d recovered=%d pending_retirements=%d\n", result.DryRun, len(result.SessionIDs), result.Recovered, result.PendingRetirements)
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
	return recoverInterruptedCanonicalMigrationWithSessionLoader(home, store, nativeRoot, state, func() ([]codex.Session, error) {
		return codex.LoadSessions(home)
	}, nil)
}

func recoverInterruptedCanonicalMigrationsWithSnapshot(home string, store string, nativeRoot string, states []vfs.SessionState, sessions []codex.Session) (retiredSessions []string, resultErr error) {
	return recoverInterruptedCanonicalMigrationsWithOwnerHandoff(home, store, nativeRoot, states, sessions, nil)
}

func recoverInterruptedCanonicalMigrationsWithOwnerHandoff(
	home string,
	store string,
	nativeRoot string,
	states []vfs.SessionState,
	sessions []codex.Session,
	handoff canonicalRetirementOwnerHandoff,
) (retiredSessions []string, resultErr error) {
	var recoveryErrors []error
	for _, state := range states {
		changed, err := recoverInterruptedCanonicalMigrationWithSnapshotAndHandoff(home, store, nativeRoot, state, sessions, handoff)
		if changed {
			retiredSessions = append(retiredSessions, state.SessionID)
		}
		if err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("recover interrupted canonical migration %s: %w", state.SessionID, err))
		}
	}
	return retiredSessions, errors.Join(recoveryErrors...)
}

func detachCompletedManagedSession(filesystem *mountfs.Filesystem, sessionID string, expectedRoute string, known map[string]uint64, knownRoutes map[string]string, knownPacks map[string]string, commit func() error) error {
	if err := filesystem.RemoveSessionAtWithCommit(sessionID, expectedRoute, commit); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("detach completed managed session %s: %w", sessionID, err)
	}
	delete(known, sessionID)
	delete(knownRoutes, sessionID)
	delete(knownPacks, sessionID)
	return nil
}

func recoverInterruptedCanonicalMigrationWithSnapshot(home string, store string, nativeRoot string, state vfs.SessionState, sessions []codex.Session) (recovered bool, resultErr error) {
	return recoverInterruptedCanonicalMigrationWithSnapshotAndHandoff(home, store, nativeRoot, state, sessions, nil)
}

func recoverInterruptedCanonicalMigrationWithSnapshotAndHandoff(home string, store string, nativeRoot string, state vfs.SessionState, sessions []codex.Session, handoff canonicalRetirementOwnerHandoff) (recovered bool, resultErr error) {
	return recoverInterruptedCanonicalMigrationWithSessionLoader(home, store, nativeRoot, state, func() ([]codex.Session, error) {
		return sessions, nil
	}, handoff)
}

func recoverInterruptedCanonicalMigrationWithSessionLoader(home string, store string, nativeRoot string, state vfs.SessionState, loadSessions func() ([]codex.Session, error), handoff canonicalRetirementOwnerHandoff) (recovered bool, resultErr error) {
	return recoverInterruptedCanonicalMigrationWithOptions(home, store, nativeRoot, state, loadSessions, handoff, nil)
}

func recoverInterruptedCanonicalMigrationWithOptions(
	home string,
	store string,
	nativeRoot string,
	state vfs.SessionState,
	loadSessions func() ([]codex.Session, error),
	handoff canonicalRetirementOwnerHandoff,
	hook canonicalRetirementRecoveryHook,
) (recovered bool, resultErr error) {
	deletionLock, err := storage.AcquireOperationLock(store, "session-deletions")
	if err != nil {
		return false, fmt.Errorf("serialize interrupted migration recovery with explicit deletion: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, deletionLock.Close()) }()
	if _, err := vfs.LoadSessionDeletion(store, state.SessionID); err == nil {
		return false, fmt.Errorf("explicit deletion tombstone supersedes interrupted canonical migration %s", state.SessionID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect deletion authority before interrupted migration recovery: %w", err)
	}
	return recoverInterruptedCanonicalMigrationWithOptionsLocked(home, store, nativeRoot, state, loadSessions, handoff, hook)
}

func recoverInterruptedCanonicalMigrationWithOptionsLocked(
	home string,
	store string,
	nativeRoot string,
	state vfs.SessionState,
	loadSessions func() ([]codex.Session, error),
	handoff canonicalRetirementOwnerHandoff,
	hook canonicalRetirementRecoveryHook,
) (recovered bool, resultErr error) {
	retainedPath := filepath.Join(store, "fs", "snapshots", state.SessionID, "native.jsonl")
	if filepath.Clean(state.NativeSnapshot.Path) != filepath.Clean(retainedPath) {
		return false, nil
	}
	if retired, err := vfs.NativeSnapshotAlreadyRetired(store, state); err != nil {
		return false, fmt.Errorf("verify native retirement before migration recovery: %w", err)
	} else if retired {
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
	sessions, err := loadSessions()
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
	// A session with writes cannot be an untouched interrupted migration.
	// Avoid hashing its potentially large native source on every reload.
	if state.BackingPath != "" {
		return false, nil
	}
	delta, deltaErr := os.Stat(state.DeltaPath)
	if deltaErr == nil && delta.Size() != 0 {
		return false, nil
	}
	source, err := hashStableCanonicalNativeFile(nativeRoot, sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if deltaErr != nil {
		return false, deltaErr
	}
	if source.Bytes != state.BaseBytes || source.SHA256 != state.BaseSHA256 || source.Bytes != state.NativeSnapshot.Bytes || source.SHA256 != state.NativeSnapshot.SHA256 {
		return false, errors.New("interrupted canonical migration source no longer matches the managed base")
	}
	relativeRoute, err := canonicalRelativeRoute(home, current.RolloutPath)
	if err != nil {
		return false, err
	}
	requestRoute := "/" + filepath.ToSlash(relativeRoute)
	request, err := createRetirementRequest(store, state.SessionID, state.Generation, requestRoute, source)
	if err != nil {
		return false, fmt.Errorf("publish interrupted migration retirement request: %w", err)
	}
	if err := runCanonicalRetirementHook(hook, state.SessionID, retirementPhaseRequestPublished); err != nil {
		return false, err
	}
	if handoff == nil {
		// A standalone CLI has no authority to detach a live filesystem owner.
		// The durable request is sufficient for the daemon to finish atomically.
		return false, nil
	}
	activeDirectory := filepath.Join(filepath.Clean(store), "fs", "sessions", state.SessionID)
	commit := func() error {
		if _, err := vfs.LoadSessionDeletion(store, state.SessionID); err == nil {
			return errors.New("explicit deletion tombstone supersedes interrupted migration cutover")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		binding, err := loadCurrentRetirementStateBinding(activeDirectory, state.SessionID, request.Generation)
		if err != nil || binding.StateSHA256 != request.StateSHA256 || binding.CheckpointSequence != request.CheckpointSequence || binding.CheckpointSHA256 != request.CheckpointSHA256 {
			return publishRetirementCutoverRejectionAt(activeDirectory, request, "managed checkpoint changed before interrupted migration cutover")
		}
		freshCurrent, err := codex.LoadSession(home, state.SessionID)
		if errors.Is(err, codex.ErrSessionNotFound) {
			return publishRetirementCutoverRejectionAt(activeDirectory, request, "session route disappeared before interrupted migration cutover")
		}
		if err != nil {
			return fmt.Errorf("refresh Codex metadata at interrupted migration cutover: %w", err)
		}
		freshRelativeRoute, err := canonicalRelativeRoute(home, freshCurrent.RolloutPath)
		if err != nil || "/"+filepath.ToSlash(freshRelativeRoute) != request.Route {
			return publishRetirementCutoverRejectionAt(activeDirectory, request, "session route changed before interrupted migration cutover")
		}
		freshSourcePath, err := canonicalNativeRoute(home, nativeRoot, freshCurrent.RolloutPath)
		if err != nil || filepath.Clean(freshSourcePath) != filepath.Clean(sourcePath) {
			return publishRetirementCutoverRejectionAt(activeDirectory, request, "native source route changed before interrupted migration cutover")
		}
		if !canonicalNativeFileStillMatches(nativeRoot, freshSourcePath, source) {
			return publishRetirementCutoverRejectionAt(activeDirectory, request, "native source changed before interrupted migration cutover")
		}
		return writeRetirementAcknowledgementAt(activeDirectory, request)
	}
	if err := handoff(state.SessionID, request.Route, commit); err != nil {
		if errors.Is(err, errRetirementCutoverRejected) {
			return false, nil
		}
		if errors.Is(err, mountfs.ErrManagedSessionRouteChanged) {
			if rejectErr := ensureRetirementRejectionAt(activeDirectory, request, "managed owner route changed before interrupted migration cutover"); rejectErr != nil {
				return false, errors.Join(err, rejectErr)
			}
			return false, nil
		}
		return false, fmt.Errorf("handoff interrupted canonical migration owner: %w", err)
	}
	if err := runCanonicalRetirementHook(hook, state.SessionID, retirementPhaseOwnerDetached); err != nil {
		return false, err
	}
	retiredState, err := retireManagedStateForRecovery(store, state.SessionID, activeDirectory)
	if err != nil {
		return false, err
	}
	if err := runCanonicalRetirementHook(hook, state.SessionID, retirementPhaseStateRetired); err != nil {
		return false, err
	}
	if _, err := retireCanonicalNativeSnapshot(store, nativeRoot, state.SessionID, state.NativeSnapshot.Path, sourcePath, retiredState); err != nil {
		return false, err
	}
	if err := runCanonicalRetirementHook(hook, state.SessionID, retirementPhaseNativeRetired); err != nil {
		return false, err
	}
	if err := removeManagedSessionRegistryEntryIfPresent(store, state.SessionID); err != nil {
		return false, fmt.Errorf("remove completed migration from durable managed session registry: %w", err)
	}
	if err := clearRetirementRequestLast(retiredState, request, true, hook, state.SessionID); err != nil {
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

func openPackOnlyVerifiedManagedSession(ctx context.Context, store string, state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
	packReport, err := pack.Doctor(ctx, store)
	if err != nil {
		return nil, nil, err
	}
	if packReport.ManifestCount == 0 || packReport.IssueCount != 0 || packReport.VerifiedManifestCount != packReport.ManifestCount {
		return nil, nil, fmt.Errorf("pack-only recovery proof failed: %d issue(s)", packReport.IssueCount)
	}
	managed, resolver, err := openManagedSession(ctx, store, state)
	if err != nil {
		return nil, nil, err
	}
	foldReport, err := fold.DoctorWithOptions(ctx, store, fold.DoctorOptions{Reader: resolver})
	if err != nil {
		_ = resolver.Close()
		return nil, nil, err
	}
	if foldReport.IssueCount != 0 || foldReport.ManifestCount != packReport.ManifestCount || foldReport.VerifiedManifestCount != foldReport.ManifestCount {
		_ = resolver.Close()
		return nil, nil, fmt.Errorf("fold pack-only recovery proof failed: %d issue(s)", foldReport.IssueCount)
	}
	return managed, resolver, nil
}

func validateRetiredNativeProofForState(store string, state vfs.SessionState, proof vfs.NativeRetirementProof) error {
	if proof.SessionID != state.SessionID {
		return errors.New("native retirement proof belongs to another managed session")
	}
	if proof.StateGeneration >= state.Generation {
		return errors.New("native retirement proof is not earlier than the current managed state")
	}
	expectedSnapshot := filepath.Join(filepath.Clean(store), "fs", "snapshots", state.SessionID, "native.jsonl")
	if filepath.Clean(proof.Snapshot.Path) != filepath.Clean(expectedSnapshot) {
		return errors.New("native retirement proof does not reference this session's canonical snapshot")
	}
	return vfs.ValidateNativeRetirementProofForState(store, state, proof)
}

func currentPackForStates(store string, states []vfs.SessionState) (string, error) {
	current, err := pack.CurrentGeneration(store)
	if err == nil {
		return current, nil
	}
	if len(states) != 0 {
		return "", err
	}
	bootstrap, bootstrapErr := pack.IsBootstrapStore(store)
	if bootstrapErr != nil {
		return "", bootstrapErr
	}
	if bootstrap {
		return "", nil
	}
	return "", err
}

func upsertCanonicalManagedState(
	store string,
	filesystem *mountfs.Filesystem,
	state vfs.SessionState,
	route string,
	known map[string]uint64,
	knownRoutes map[string]string,
	knownPacks map[string]string,
	openState func(vfs.SessionState) (*vfs.Session, *pack.Resolver, error),
) error {
	managed, resolver, err := openState(state)
	if err != nil {
		return err
	}
	if err := filesystem.UpsertSessionAtOwned(state.SessionID, route, managed, resolver); err != nil {
		return err
	}
	recovered := managed.State()
	if recovered.SessionID != state.SessionID || recovered.Generation == 0 {
		return errors.New("opened managed session returned an invalid recovered state")
	}
	if err := writeMountAcknowledgement(store, state.SessionID, recovered.Generation, route); err != nil {
		return err
	}
	known[state.SessionID] = recovered.Generation
	knownRoutes[state.SessionID] = route
	knownPacks[state.SessionID] = resolver.Generation()
	return nil
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
	foldReport, err := doctorFoldStore(ctx, store)
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
	result, err := storage.Collect(ctx, storage.GCOptions{
		StoreDir: store, Apply: true,
		AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
			return pack.AuthorizeGenerationRemoval(ctx, store, candidate)
		},
	})
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
		result, ran, err := run(ctx, store)
		if err != nil && !errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintf(diagnostics, "storage maintenance failed: %v\n", err)
		} else if ran && result.RetainedUnprovedCount != 0 {
			_, _ = fmt.Fprintf(diagnostics, "storage maintenance retained %d candidate(s) (%d apparent bytes) without exact durable deletion proof\n", result.RetainedUnprovedCount, result.RetainedUnprovedApparentBytes)
		}
	}()
	return done
}

func storageStatusSnapshot(inventory storage.Inventory, scanErr error, observedAt time.Time) fskitstatus.Snapshot {
	snapshot := fskitstatus.Snapshot{
		Component: "storage",
		State:     "healthy",
		UpdatedAt: observedAt.UTC().Format(time.RFC3339Nano),
		Summary:   "Session storage accounting is available",
	}
	if scanErr != nil {
		snapshot.State = "recovering"
		snapshot.Summary = "Session storage accounting is temporarily unavailable"
		snapshot.Detail = scanErr.Error()
		return snapshot
	}
	logicalBytes := inventory.LogicalSessionBytes
	physicalBytes := inventory.TotalPhysicalBytes
	snapshot.LogicalBytes = &logicalBytes
	snapshot.PhysicalBytes = &physicalBytes
	if inventory.IssueCount != 0 {
		snapshot.State = "recovering"
		snapshot.Summary = "Session storage accounting needs review"
		snapshot.Detail = fmt.Sprintf("storage inventory reported %d issue(s)", inventory.IssueCount)
	}
	return snapshot
}

func startStorageStatusReporter(
	ctx context.Context,
	diagnostics io.Writer,
	store string,
	statusPath string,
	interval time.Duration,
	after <-chan struct{},
	scan func(context.Context, storage.Options) (storage.Inventory, error),
	publish func(string, fskitstatus.Snapshot) error,
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if after != nil {
			select {
			case <-ctx.Done():
				return
			case <-after:
			}
		}
		if interval <= 0 {
			interval = time.Minute
		}
		publishCurrent := func() {
			inventory, err := scan(ctx, storage.Options{StoreDir: store, AllowMetadataIssues: true})
			if errors.Is(err, context.Canceled) {
				return
			}
			if publishErr := publish(statusPath, storageStatusSnapshot(inventory, err, time.Now())); publishErr != nil {
				_, _ = fmt.Fprintf(diagnostics, "write storage status: %v\n", publishErr)
			}
		}
		publishCurrent()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				publishCurrent()
			}
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
			report, err := doctorFoldStore(ctx, store)
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
				if state.NativeSnapshot.Path != "" {
					if _, err := os.Stat(state.NativeSnapshot.Path); err != nil {
						return err
					}
					continue
				}
				proof, err := vfs.LoadNativeRetirementProof(filepath.Join(store, "fs", "sessions", state.SessionID, vfs.NativeRetirementFilename))
				if err != nil || proof.SessionID != state.SessionID || proof.StateGeneration > state.Generation {
					if err == nil {
						err = errors.New("native retirement proof does not match managed state")
					}
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
		fsctl.Check{Component: fsctl.ComponentClient, NonBlocking: true, Run: func(ctx context.Context) error {
			result, err := evaluateCompatibility(ctx, store, defaultCompatibilityFlags())
			if err != nil {
				return err
			}
			if len(result.DetectionErrors) != 0 {
				return fmt.Errorf("detect installed Codex clients: %s", strings.Join(result.DetectionErrors, "; "))
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

func waitForTargetPrefix(ctx context.Context, target string, expected vfs.NativeFile, timeout time.Duration) (vfs.NativeFile, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		file, matches, err := hashPathPrefix(target, expected.Bytes, expected.SHA256)
		if err == nil && matches {
			return file, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return vfs.NativeFile{}, err
		}
		if time.Now().After(deadline) {
			return vfs.NativeFile{}, errors.New("timed out waiting for committed mounted-session prefix")
		}
		select {
		case <-ctx.Done():
			return vfs.NativeFile{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func hashPathPrefix(path string, expectedBytes int64, expectedSHA256 string) (vfs.NativeFile, bool, error) {
	if expectedBytes < 0 || !validRetirementSHA256(expectedSHA256) {
		return vfs.NativeFile{}, false, errors.New("valid prefix identity is required")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return vfs.NativeFile{}, false, err
	}
	if !before.Mode().IsRegular() || before.Size() < expectedBytes {
		return vfs.NativeFile{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return vfs.NativeFile{}, false, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return vfs.NativeFile{}, false, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() < expectedBytes {
		_ = file.Close()
		return vfs.NativeFile{}, false, errors.New("prefix target changed while it was opened")
	}
	hasher := sha256.New()
	bytesRead, readErr := io.CopyN(hasher, file, expectedBytes)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return vfs.NativeFile{}, false, err
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() < expectedBytes {
		if err == nil {
			err = errors.New("prefix target changed while it was read")
		}
		return vfs.NativeFile{}, false, err
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	return vfs.NativeFile{Path: path, Bytes: after.Size(), SHA256: digest}, bytesRead == expectedBytes && digest == expectedSHA256, nil
}

func hashManagedSessionVisible(ctx context.Context, session *vfs.Session) (vfs.NativeFile, error) {
	if session == nil {
		return vfs.NativeFile{}, errors.New("managed session is required")
	}
	reader, err := session.OpenReader()
	if err != nil {
		return vfs.NativeFile{}, err
	}
	size := reader.Size()
	hasher := sha256.New()
	buffer := make([]byte, 64<<10)
	var offset int64
	for offset < size {
		chunk := buffer
		if remaining := size - offset; int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		n, readErr := reader.ReadAt(ctx, chunk, offset)
		if n > 0 {
			_, _ = hasher.Write(chunk[:n])
			offset += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return vfs.NativeFile{}, errors.Join(readErr, reader.Close())
		}
		if n == 0 {
			return vfs.NativeFile{}, errors.Join(io.ErrUnexpectedEOF, reader.Close())
		}
	}
	if err := reader.Close(); err != nil {
		return vfs.NativeFile{}, err
	}
	return vfs.NativeFile{Bytes: size, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func waitForCanonicalRetirementCompletion(ctx context.Context, store string, sessionID string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		pending, err := discoverPendingCanonicalRetirements(store)
		if err != nil {
			return "", err
		}
		stillPending := false
		for _, transaction := range pending {
			if transaction.SessionID == sessionID {
				stillPending = true
				break
			}
		}
		activePath := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
		_, activeErr := os.Lstat(activePath)
		if !stillPending && errors.Is(activeErr, os.ErrNotExist) {
			retired, globErr := filepath.Glob(filepath.Join(filepath.Clean(store), "fs", "retired", sessionID+"-*"))
			if globErr != nil {
				return "", globErr
			}
			if len(retired) != 0 {
				sort.Strings(retired)
				return retired[len(retired)-1], nil
			}
		} else if activeErr != nil && !errors.Is(activeErr, os.ErrNotExist) {
			return "", activeErr
		}
		if time.Now().After(deadline) {
			return "", errors.New("timed out waiting for request-last retirement completion")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
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
