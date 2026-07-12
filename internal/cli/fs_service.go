package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/mountfs"
	"github.com/jstar0/codexfold/internal/service"
	"github.com/jstar0/codexfold/internal/vfs"
	"github.com/spf13/cobra"
)

const serviceLabel = "com.codexfold.fs"

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
	var codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "install",
		Short: "Render and optionally bootstrap a per-user launchd service",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, store, mount, binary, plist, logs, err := resolveServicePaths(codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir)
			if err != nil {
				return err
			}
			definition, err := service.RenderLaunchd(service.Options{Label: serviceLabel, BinaryPath: binary, CodexHome: home, StoreDir: store, MountPoint: mount, StdoutPath: filepath.Join(logs, "stdout.log"), StderrPath: filepath.Join(logs, "stderr.log")})
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
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t path=%s bytes=%d\n", result.DryRun, result.Path, result.Bytes)
			return err
		},
	}
	addServicePathFlags(command, &codexHome, &storeDir, &mountPoint, &binaryPath, &plistPath, &logDir)
	command.Flags().BoolVar(&apply, "apply", false, "Write, bootstrap, and start the per-user service")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSServiceStartCommand() *cobra.Command {
	return newFSServiceLifecycleCommand("start", func(ctx context.Context, manager service.Manager, plist string) error {
		_ = manager.Bootout(ctx, plist)
		if err := manager.Bootstrap(ctx, plist); err != nil {
			return err
		}
		return manager.Kickstart(ctx, serviceLabel)
	})
}

func newFSServiceStopCommand() *cobra.Command {
	return newFSServiceLifecycleCommand("stop", func(ctx context.Context, manager service.Manager, plist string) error {
		return manager.Bootout(ctx, plist)
	})
}

func newFSServiceLifecycleCommand(action string, run func(context.Context, service.Manager, string) error) *cobra.Command {
	var plistPath string
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
				if err := run(command.Context(), service.Manager{}, plist); err != nil {
					return err
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
		if isGeneratedNativeFallbackPath(current.RolloutPath, store, state.SessionID) {
			if _, err := hashPath(current.RolloutPath); err != nil {
				return false, err
			}
			continue
		}
		managed, resolver, err := openManagedSession(ctx, store, state)
		if err != nil {
			return false, err
		}
		visible, err := hashManagedSession(ctx, managed)
		_ = resolver.Close()
		if err != nil {
			return false, err
		}
		route, err := hashPath(current.RolloutPath)
		if err != nil || route.Bytes != visible.Bytes || route.SHA256 != visible.SHA256 {
			return false, nil
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
		targetPath := filepath.Join(store, "fs", "sessions", state.SessionID, "quarantine-current.jsonl")
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
		count++
	}
	return count, nil
}

func isGeneratedNativeFallbackPath(path string, store string, sessionID string) bool {
	if path == "" || store == "" || sessionID == "" {
		return false
	}
	if filepath.Clean(filepath.Dir(path)) != filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID) {
		return false
	}
	switch filepath.Base(path) {
	case "fallback-current.jsonl", "quarantine-current.jsonl":
		return true
	default:
		return false
	}
}

func hashManagedSession(ctx context.Context, session *vfs.Session) (vfs.NativeFile, error) {
	reader, err := session.OpenReader()
	if err != nil {
		return vfs.NativeFile{}, err
	}
	defer reader.Close()
	hasher := sha256.New()
	buffer := make([]byte, 1<<20)
	var offset int64
	for offset < reader.Size() {
		need := len(buffer)
		if remaining := reader.Size() - offset; int64(need) > remaining {
			need = int(remaining)
		}
		n, readErr := reader.ReadAt(ctx, buffer[:need], offset)
		if n > 0 {
			_, _ = hasher.Write(buffer[:n])
			offset += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return vfs.NativeFile{}, readErr
		}
		if n == 0 {
			break
		}
	}
	if offset != reader.Size() {
		return vfs.NativeFile{}, errors.New("managed session ended before its declared size")
	}
	return vfs.NativeFile{Bytes: offset, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
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
