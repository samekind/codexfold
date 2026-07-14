package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/mountfs"
	"github.com/jstar0/codexfold/internal/service"
	"github.com/jstar0/codexfold/internal/vfs"
	"github.com/spf13/cobra"
)

const serviceLabel = "com.codexfold.fs"

type operationTrace struct {
	mu   sync.Mutex
	file *os.File
}

func newOperationRecorder(path string) (func(string), io.Closer, error) {
	if !filepath.IsAbs(path) {
		return nil, nil, errors.New("operation trace path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	trace := &operationTrace{file: file}
	return trace.record, trace, nil
}

func (t *operationTrace) record(operation string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = fmt.Fprintf(t.file, "%d %s\n", time.Now().UnixNano(), operation)
}

func (t *operationTrace) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.file.Sync(); err != nil {
		_ = t.file.Close()
		return err
	}
	return t.file.Close()
}

type FSServiceActionResult struct {
	Action string `json:"action"`
	Path   string `json:"path,omitempty"`
	DryRun bool   `json:"dry_run"`
}

type FSUpdatePreflightResult struct {
	DoctorHealthy       bool                   `json:"doctor_healthy"`
	Compatibility       FSCompatibilityResult  `json:"compatibility"`
	Decision            service.UpdateDecision `json:"decision"`
	QuarantinedSessions int                    `json:"quarantined_sessions"`
}

func newFSServiceCommand() *cobra.Command {
	command := &cobra.Command{Use: "service", Short: "Manage the per-user transparent filesystem service"}
	command.AddCommand(newFSServiceInstallCommand())
	command.AddCommand(newFSServiceStartCommand())
	command.AddCommand(newFSServiceStopCommand())
	command.AddCommand(newFSServiceStatusCommand())
	command.AddCommand(newFSServiceUpdatePreflightCommand())
	return command
}

func newFSServiceInstallCommand() *cobra.Command {
	var codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir, nativeRoot, operationTracePath string
	var apply, canonicalNamespace, jsonOutput bool
	command := &cobra.Command{
		Use:   "install",
		Short: "Render and optionally bootstrap a per-user launchd service",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, store, mount, binary, plist, logs, err := resolveServicePaths(codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir)
			if err != nil {
				return err
			}
			if canonicalNamespace {
				if nativeRoot == "" {
					nativeRoot = filepath.Join(home, "fold-native")
				}
				if !filepath.IsAbs(nativeRoot) {
					return errors.New("canonical service native root must be absolute")
				}
				nativeRoot = filepath.Clean(nativeRoot)
			}
			definition, err := service.RenderLaunchd(service.Options{
				Label: serviceLabel, BinaryPath: binary, CodexHome: home, StoreDir: store, MountPoint: mount,
				StdoutPath: filepath.Join(logs, "stdout.log"), StderrPath: filepath.Join(logs, "stderr.log"),
				CanonicalNamespace: canonicalNamespace, NativeRoot: nativeRoot, OperationTrace: operationTracePath,
			})
			if err != nil {
				return err
			}
			if apply && (runtime.GOOS != "darwin" || !mountfs.Available()) {
				return errors.New("service installation requires a FUSE-enabled macOS build and an authorized host prerequisite")
			}
			if apply {
				if err := os.MkdirAll(logs, 0o700); err != nil {
					return err
				}
			}
			result, err := service.WriteDefinition(plist, definition, apply)
			if err != nil {
				return err
			}
			if apply {
				manager := service.Manager{}
				_ = manager.Bootout(command.Context(), plist)
				if err := manager.Bootstrap(command.Context(), plist); err != nil {
					return err
				}
				if err := manager.Kickstart(command.Context(), serviceLabel); err != nil {
					return err
				}
				if _, err := manager.WaitHealthy(command.Context(), serviceLabel, mount, 15*time.Second); err != nil {
					_ = manager.Bootout(command.Context(), plist)
					return err
				}
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t path=%s bytes=%d\n", result.DryRun, result.Path, result.Bytes)
			return err
		},
	}
	addServicePathFlags(command, &codexHome, &storeDir, &mountPoint, &binaryPath, &plistPath, &logDir)
	command.Flags().BoolVar(&canonicalNamespace, "canonical-namespace", false, "Start the service with the canonical Codex session namespace")
	command.Flags().StringVar(&nativeRoot, "native-root", "", "Canonical native backing root; defaults to <codex-home>/fold-native")
	command.Flags().StringVar(&operationTracePath, "operation-trace", "", "Absolute path for sanitized FUSE operation names")
	command.Flags().BoolVar(&apply, "apply", false, "Write, bootstrap, and start the per-user service")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSServiceStartCommand() *cobra.Command {
	return newFSServiceLifecycleCommand("start", true, func(ctx context.Context, manager service.Manager, plist string) error {
		_ = manager.Bootout(ctx, plist)
		if err := manager.Bootstrap(ctx, plist); err != nil {
			return err
		}
		return manager.Kickstart(ctx, serviceLabel)
	})
}

func newFSServiceStopCommand() *cobra.Command {
	return newFSServiceLifecycleCommand("stop", false, func(ctx context.Context, manager service.Manager, plist string) error {
		return manager.Bootout(ctx, plist)
	})
}

func newFSServiceLifecycleCommand(action string, waitForMount bool, run func(context.Context, service.Manager, string) error) *cobra.Command {
	var plistPath, codexHome, mountPoint string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   action,
		Short: action + " the per-user filesystem service",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			plist, err := resolvePlistPath(plistPath)
			if err != nil {
				return err
			}
			result := FSServiceActionResult{Action: action, Path: plist, DryRun: !apply}
			if apply {
				if runtime.GOOS != "darwin" {
					return errors.New("launchd service lifecycle is available only on macOS")
				}
				manager := service.Manager{}
				if err := run(command.Context(), manager, plist); err != nil {
					return err
				}
				if waitForMount {
					home, err := codex.ResolveHome(codexHome)
					if err != nil {
						return err
					}
					if _, err := manager.WaitHealthy(command.Context(), serviceLabel, defaultMountPoint(home, mountPoint), 15*time.Second); err != nil {
						_ = manager.Bootout(command.Context(), plist)
						return err
					}
				}
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "action=%s dry_run=%t path=%s\n", action, result.DryRun, plist)
			return err
		},
	}
	command.Flags().StringVar(&plistPath, "plist", "", "LaunchAgent plist path")
	if waitForMount {
		command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
		command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	}
	command.Flags().BoolVar(&apply, "apply", false, "Execute the launchctl action")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSServiceStatusCommand() *cobra.Command {
	var codexHome, mountPoint string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Report daemon and mount health separately",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			status := service.Manager{}.Status(command.Context(), serviceLabel, defaultMountPoint(home, mountPoint))
			if jsonOutput {
				return writeJSON(command, status)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "daemon=%t mount=%t daemon_error=%q mount_error=%q\n", status.DaemonRunning, status.MountHealthy, status.DaemonError, status.MountError)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSServiceUpdatePreflightCommand() *cobra.Command {
	var codexHome, storeDir string
	var compatibility compatibilityFlags
	var automatic, promote, applyQuarantine, jsonOutput bool
	command := &cobra.Command{
		Use:   "update-preflight",
		Short: "Gate service updates and optionally route unknown-version sessions to current native bytes",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			doctorErr := requireStorageHealth(command.Context(), store)
			compatibilityResult, err := evaluateCompatibility(command.Context(), store, compatibility)
			if err != nil {
				return err
			}
			fallbackReady, err := managedRoutesMatchCurrentBytes(command.Context(), home, store)
			if err != nil {
				fallbackReady = false
			}
			decision := service.EvaluateUpdate(service.UpdateInput{Capability: verifiedCapability(), DoctorHealthy: doctorErr == nil, Compatibility: compatibilityResult.Evaluation, NativeFallbackReady: fallbackReady, Automatic: automatic, ExplicitPromotion: promote})
			result := FSUpdatePreflightResult{DoctorHealthy: doctorErr == nil, Compatibility: compatibilityResult, Decision: decision}
			if decision.Quarantine && decision.RequiresNativeFallback && applyQuarantine {
				count, err := quarantineManagedRoutes(command.Context(), home, store)
				if err != nil {
					return err
				}
				result.QuarantinedSessions = count
				result.Decision = service.EvaluateUpdate(service.UpdateInput{Capability: verifiedCapability(), DoctorHealthy: doctorErr == nil, Compatibility: compatibilityResult.Evaluation, NativeFallbackReady: true, Automatic: automatic, ExplicitPromotion: promote})
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "allowed=%t quarantine=%t requires_fallback=%t doctor=%t quarantined=%d reason=%q\n", result.Decision.Allowed, result.Decision.Quarantine, result.Decision.RequiresNativeFallback, result.DoctorHealthy, result.QuarantinedSessions, result.Decision.Reason)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	addCompatibilityFlags(command, &compatibility)
	command.Flags().BoolVar(&automatic, "automatic", false, "Evaluate an unattended update")
	command.Flags().BoolVar(&promote, "promote", false, "Explicitly approve preview or canary promotion")
	command.Flags().BoolVar(&applyQuarantine, "apply-quarantine", false, "Route managed sessions to verified current native bytes when clients are unknown")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func managedRoutesMatchCurrentBytes(ctx context.Context, home string, store string) (bool, error) {
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		return false, err
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return false, err
	}
	byID := make(map[string]codex.Session, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}
	for _, state := range states {
		current, ok := byID[state.SessionID]
		if !ok {
			return false, fmt.Errorf("Codex route missing for managed session %s", state.SessionID)
		}
		if !isGeneratedNativeFallbackPath(current.RolloutPath, store, state.SessionID) {
			return false, nil
		}
		if _, err := hashPath(current.RolloutPath); err != nil {
			return false, err
		}
	}
	return true, nil
}

func quarantineManagedRoutes(ctx context.Context, home string, store string) (int, error) {
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		return 0, err
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]codex.Session, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}
	count := 0
	for _, state := range states {
		current, ok := byID[state.SessionID]
		if !ok {
			return count, fmt.Errorf("Codex route missing for managed session %s", state.SessionID)
		}
		if isGeneratedNativeFallbackPath(current.RolloutPath, store, state.SessionID) {
			if _, err := hashPath(current.RolloutPath); err != nil {
				return count, err
			}
			continue
		}
		managed, resolver, err := openManagedSession(ctx, store, state)
		if err != nil {
			return count, err
		}
		targetDirectory := filepath.Join(store, "fs", "fallbacks", state.SessionID)
		if err := os.MkdirAll(targetDirectory, 0o700); err != nil {
			return count, err
		}
		if err := os.Chmod(targetDirectory, 0o700); err != nil {
			return count, err
		}
		targetPath := filepath.Join(targetDirectory, "quarantine-current.jsonl")
		target, err := managed.MaterializeCurrent(ctx, targetPath, true)
		_ = resolver.Close()
		if err != nil {
			return count, err
		}
		if filepath.Clean(current.RolloutPath) == filepath.Clean(target.Path) {
			continue
		}
		if _, err := codex.RouteSession(ctx, codex.RouteOptions{CodexHome: home, SessionID: state.SessionID, ExpectedPath: current.RolloutPath, Target: codex.RouteTarget{Path: target.Path, Bytes: target.Bytes, SHA256: target.SHA256}}); err != nil {
			return count, err
		}
		if _, err := retireManagedState(store, state.SessionID); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func isGeneratedNativeFallbackPath(path string, store string, sessionID string) bool {
	if path == "" || store == "" || sessionID == "" {
		return false
	}
	directory := filepath.Clean(filepath.Dir(path))
	legacyDirectory := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	fallbackDirectory := filepath.Join(filepath.Clean(store), "fs", "fallbacks", sessionID)
	if directory != legacyDirectory && directory != fallbackDirectory {
		return false
	}
	switch filepath.Base(path) {
	case "fallback-current.jsonl", "quarantine-current.jsonl":
		return true
	default:
		return false
	}
}

func retireManagedState(store string, sessionID string) (string, error) {
	if store == "" || sessionID == "" {
		return "", errors.New("store and session ID are required")
	}
	source := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	retiredRoot := filepath.Join(filepath.Clean(store), "fs", "retired")
	if err := os.MkdirAll(retiredRoot, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(retiredRoot, fmt.Sprintf("%s-%d", sessionID, time.Now().UnixNano()))
	if err := os.Rename(source, target); err != nil {
		return "", err
	}
	return target, nil
}

func restoreManagedState(store string, sessionID string, retiredPath string) error {
	if store == "" || sessionID == "" || retiredPath == "" {
		return errors.New("store, session ID, and retired state path are required")
	}
	target := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	if err := os.Rename(filepath.Clean(retiredPath), target); err != nil {
		return err
	}
	if _, err := vfs.RepublishSessionState(filepath.Join(target, "state.json")); err != nil {
		return fmt.Errorf("republish restored managed state: %w", err)
	}
	return nil
}

func retainCanonicalSnapshot(store string, sessionID string, source vfs.NativeFile) (vfs.NativeFile, error) {
	if store == "" || !validSessionID(sessionID) || source.Path == "" {
		return vfs.NativeFile{}, errors.New("store, session ID, and source snapshot are required")
	}
	sourcePath := filepath.Clean(source.Path)
	verified, err := hashPath(sourcePath)
	if err != nil {
		return vfs.NativeFile{}, fmt.Errorf("verify canonical native snapshot: %w", err)
	}
	if verified.Bytes != source.Bytes || verified.SHA256 != source.SHA256 {
		return vfs.NativeFile{}, errors.New("canonical native snapshot changed during migration")
	}
	retainedDir := filepath.Join(filepath.Clean(store), "fs", "snapshots", sessionID)
	retainedPath := filepath.Join(retainedDir, "native.jsonl")
	if err := os.MkdirAll(retainedDir, 0o700); err != nil {
		return vfs.NativeFile{}, err
	}
	if _, err := os.Lstat(retainedPath); err == nil {
		return vfs.NativeFile{}, errors.New("retained canonical snapshot already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return vfs.NativeFile{}, err
	}
	if err := os.Link(sourcePath, retainedPath); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return vfs.NativeFile{}, fmt.Errorf("stage canonical native snapshot: %w", err)
		}
		if err := copyCanonicalSnapshot(sourcePath, retainedPath); err != nil {
			return vfs.NativeFile{}, fmt.Errorf("copy canonical native snapshot: %w", err)
		}
	}
	retained, err := hashPath(retainedPath)
	if err == nil && (retained.Bytes != source.Bytes || retained.SHA256 != source.SHA256) {
		err = errors.New("retained canonical snapshot does not match source")
	}
	if err != nil {
		_ = os.Remove(retainedPath)
		return vfs.NativeFile{}, err
	}
	retained.Path = retainedPath
	return retained, nil
}

func finalizeCanonicalSnapshotSource(sourcePath string, retained vfs.NativeFile) error {
	sourcePath = filepath.Clean(sourcePath)
	retained.Path = filepath.Clean(retained.Path)
	source, err := hashPath(sourcePath)
	if err != nil {
		return fmt.Errorf("verify canonical source before cutover: %w", err)
	}
	hidden, err := hashPath(retained.Path)
	if err != nil {
		return fmt.Errorf("verify retained canonical snapshot before cutover: %w", err)
	}
	if source.Bytes != retained.Bytes || source.SHA256 != retained.SHA256 || hidden.Bytes != retained.Bytes || hidden.SHA256 != retained.SHA256 {
		return errors.New("canonical source changed before cutover")
	}
	if err := os.Remove(sourcePath); err != nil {
		return fmt.Errorf("hide canonical source after mount acknowledgement: %w", err)
	}
	return nil
}

func copyCanonicalSnapshot(sourcePath string, retainedPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(retainedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		_ = os.Remove(retainedPath)
		return err
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		_ = os.Remove(retainedPath)
		return err
	}
	return target.Close()
}

func restoreCanonicalSnapshotSource(originalPath string, retainedPath string) error {
	originalPath = filepath.Clean(originalPath)
	retainedPath = filepath.Clean(retainedPath)
	if originalPath == "" || retainedPath == "" {
		return errors.New("original and retained snapshot paths are required")
	}
	if _, err := os.Lstat(originalPath); err == nil {
		original, originalErr := hashPath(originalPath)
		retained, retainedErr := hashPath(retainedPath)
		if originalErr != nil || retainedErr != nil || original.Bytes != retained.Bytes || original.SHA256 != retained.SHA256 {
			return errors.New("cannot discard retained snapshot while canonical source differs")
		}
		if err := os.Remove(retainedPath); err != nil {
			return err
		}
		_ = os.Remove(filepath.Dir(retainedPath))
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(originalPath), 0o700); err != nil {
		return err
	}
	if err := os.Rename(retainedPath, originalPath); err != nil {
		return fmt.Errorf("restore canonical native snapshot: %w", err)
	}
	return nil
}

type mountAcknowledgement struct {
	Generation uint64 `json:"generation"`
	Route      string `json:"route"`
}

func writeMountAcknowledgement(store string, sessionID string, generation uint64, route string) error {
	if store == "" || !validSessionID(sessionID) || generation == 0 || route == "" {
		return errors.New("complete mount acknowledgement metadata is required")
	}
	directory := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	data, err := json.Marshal(mountAcknowledgement{Generation: generation, Route: route})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".mounted-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(directory, "mounted.json"))
}

func waitForMountAcknowledgement(ctx context.Context, store string, sessionID string, generation uint64, route string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	path := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID, "mounted.json")
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var acknowledgement mountAcknowledgement
			if json.Unmarshal(data, &acknowledgement) == nil && acknowledgement.Generation == generation && acknowledgement.Route == route {
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the filesystem daemon")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func retireCanonicalNativeSnapshot(store string, nativeRoot string, sessionID string, snapshotPath string, currentPath string, retiredState string) (string, error) {
	snapshotPath = filepath.Clean(snapshotPath)
	if snapshotPath == filepath.Clean(currentPath) {
		return "", nil
	}
	var relative string
	legacyRelative, legacyErr := relativeWithin(filepath.Clean(nativeRoot), snapshotPath)
	hiddenRoot := filepath.Join(filepath.Clean(store), "fs", "snapshots", sessionID)
	_, hiddenErr := relativeWithin(hiddenRoot, snapshotPath)
	switch {
	case legacyErr == nil:
		relative = legacyRelative
	case hiddenErr == nil && filepath.Base(snapshotPath) == "native.jsonl":
		relative = filepath.Join("store-snapshot", "native.jsonl")
	default:
		return "", errors.New("canonical native snapshot is outside the retained snapshot roots")
	}
	target := filepath.Join(filepath.Clean(retiredState), "retained-native", relative)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(snapshotPath, target); err != nil {
		return "", err
	}
	oldSidecar := filepath.Join(filepath.Dir(snapshotPath), "._"+filepath.Base(snapshotPath))
	newSidecar := filepath.Join(filepath.Dir(target), "._"+filepath.Base(target))
	if _, err := os.Lstat(oldSidecar); err == nil {
		if err := os.Rename(oldSidecar, newSidecar); err != nil {
			_ = os.Rename(target, snapshotPath)
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Rename(target, snapshotPath)
		return "", err
	}
	if hiddenErr == nil {
		_ = os.Remove(hiddenRoot)
	}
	return target, nil
}

func validSessionID(sessionID string) bool {
	return sessionID != "" && sessionID != "." && sessionID != ".." && !strings.ContainsAny(sessionID, "/\\\x00")
}

func relativeWithin(root string, target string) (string, error) {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path is outside root")
	}
	return relative, nil
}

func restoreCanonicalNativeSnapshot(snapshotPath string, retiredSnapshot string) error {
	if retiredSnapshot == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		return err
	}
	if err := os.Rename(retiredSnapshot, snapshotPath); err != nil {
		return err
	}
	retiredSidecar := filepath.Join(filepath.Dir(retiredSnapshot), "._"+filepath.Base(retiredSnapshot))
	originalSidecar := filepath.Join(filepath.Dir(snapshotPath), "._"+filepath.Base(snapshotPath))
	if _, err := os.Lstat(retiredSidecar); err == nil {
		return os.Rename(retiredSidecar, originalSidecar)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func canonicalMountRoute(home string, mount string, route string) (string, error) {
	relative, err := canonicalRelativeRoute(home, route)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(mount), relative), nil
}

func canonicalNativeRoute(home string, nativeRoot string, route string) (string, error) {
	relative, err := canonicalRelativeRoute(home, route)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(nativeRoot) {
		return "", errors.New("canonical native root must be absolute")
	}
	return filepath.Join(filepath.Clean(nativeRoot), relative), nil
}

func canonicalNamespaceRoute(home string, mount string, route string) (string, error) {
	relative, homeErr := canonicalRelativeRoute(home, route)
	if homeErr != nil {
		var mountErr error
		relative, mountErr = canonicalRelativeRoute(mount, route)
		if mountErr != nil {
			return "", homeErr
		}
	}
	return "/" + filepath.ToSlash(relative), nil
}

func canonicalSessionRoutes(home string, mount string, store string, states []vfs.SessionState, sessions []codex.Session) (map[string]string, error) {
	managed := make(map[string]struct{}, len(states))
	for _, state := range states {
		managed[state.SessionID] = struct{}{}
	}
	routes := make(map[string]string, len(states))
	for _, session := range sessions {
		if _, exists := managed[session.ID]; !exists {
			continue
		}
		if isGeneratedNativeFallbackPath(session.RolloutPath, store, session.ID) {
			continue
		}
		route, err := canonicalNamespaceRoute(home, mount, session.RolloutPath)
		if err != nil {
			return nil, err
		}
		routes[session.ID] = route
	}
	return routes, nil
}

func discoverCanonicalRoutes(home string, mount string, store string, states []vfs.SessionState, load func(string) ([]codex.Session, error)) (map[string]string, error) {
	if len(states) == 0 {
		return map[string]string{}, nil
	}
	sessions, err := load(home)
	if err != nil {
		return nil, err
	}
	return canonicalSessionRoutes(home, mount, store, states, sessions)
}

func canonicalRelativeRoute(home string, route string) (string, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(route) {
		return "", errors.New("canonical Codex and route paths must be absolute")
	}
	home = filepath.Clean(home)
	relative, err := filepath.Rel(home, filepath.Clean(route))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("Codex route is outside its home directory")
	}
	first, remainder := relative, ""
	if separator := strings.IndexByte(relative, byte(filepath.Separator)); separator >= 0 {
		first, remainder = relative[:separator], relative[separator+1:]
	}
	if (first != "sessions" && first != "archived_sessions") || remainder == "" || !strings.HasSuffix(remainder, ".jsonl") {
		return "", errors.New("Codex route is not inside sessions or archived_sessions")
	}
	return relative, nil
}

func addServicePathFlags(command *cobra.Command, codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir *string) {
	command.Flags().StringVar(codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().StringVar(binaryPath, "binary", "", "Absolute CodexFold binary path; defaults to the current executable")
	command.Flags().StringVar(plistPath, "plist", "", "LaunchAgent plist path")
	command.Flags().StringVar(logDir, "log-dir", "", "Service log directory; defaults to <store>/service/logs")
}

func resolveServicePaths(codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir string) (string, string, string, string, string, string, error) {
	home, err := codex.ResolveHome(codexHome)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	store := resolveFoldStore(home, storeDir)
	mount := defaultMountPoint(home, mountPoint)
	binary := binaryPath
	if binary == "" {
		binary, err = os.Executable()
		if err != nil {
			return "", "", "", "", "", "", err
		}
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	plist, err := resolvePlistPath(plistPath)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	logs := logDir
	if logs == "" {
		logs = filepath.Join(store, "service", "logs")
	}
	logs, err = filepath.Abs(logs)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	return home, store, mount, binary, plist, logs, nil
}

func resolvePlistPath(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
}
