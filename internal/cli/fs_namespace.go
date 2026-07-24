package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/sessionns"
	"github.com/samekind/codexfold/internal/vfs"
	"github.com/spf13/cobra"
)

type FSNamespaceResult struct {
	sessionns.Result
	DryRun bool `json:"dry_run"`
}

func newFSNamespaceCommand() *cobra.Command {
	command := &cobra.Command{Use: "namespace", Short: "Manage the canonical Codex session directory namespace"}
	command.AddCommand(newFSNamespaceStatusCommand())
	command.AddCommand(newFSNamespaceActivateCommand())
	command.AddCommand(newFSNamespaceDeactivateCommand())
	command.AddCommand(newFSNamespaceRecoverCommand())
	return command
}

func newFSNamespaceStatusCommand() *cobra.Command {
	var codexHome, mountPoint, nativeRoot string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Inspect the canonical namespace without changing it",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			options, err := namespaceOptions(codexHome, mountPoint, nativeRoot)
			if err != nil {
				return err
			}
			result, err := sessionns.Inspect(options)
			if err != nil {
				return err
			}
			return writeNamespaceResult(command, FSNamespaceResult{Result: result, DryRun: true}, jsonOutput)
		},
	}
	addNamespaceFlags(command, &codexHome, &mountPoint, &nativeRoot)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSNamespaceActivateCommand() *cobra.Command {
	var codexHome, mountPoint, nativeRoot string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "activate",
		Short: "Atomically route Codex session directories through the canonical mount",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			options, err := namespaceOptions(codexHome, mountPoint, nativeRoot)
			if err != nil {
				return err
			}
			if apply {
				if err := requireFilesystemActivationAllowed(options.Home); err != nil {
					return err
				}
			}
			if !apply {
				result, err := sessionns.Inspect(options)
				if err != nil {
					return err
				}
				return writeNamespaceResult(command, FSNamespaceResult{Result: result, DryRun: true}, jsonOutput)
			}
			if err := mountHealthProbe(options.Mount); err != nil {
				return fmt.Errorf("canonical filesystem mount is not healthy: %w", err)
			}
			result, err := sessionns.Activate(options)
			if err != nil {
				return err
			}
			if err := waitForCanonicalNamespaceActivation(command.Context(), options.Mount, options.NativeRoot, 30*time.Second); err != nil {
				_, rollbackErr := sessionns.Deactivate(options)
				return errors.Join(fmt.Errorf("wait for canonical namespace passthrough: %w", err), rollbackErr)
			}
			return writeNamespaceResult(command, FSNamespaceResult{Result: result}, jsonOutput)
		},
	}
	addNamespaceFlags(command, &codexHome, &mountPoint, &nativeRoot)
	command.Flags().BoolVar(&apply, "apply", false, "Move native directories and install canonical links")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSNamespaceDeactivateCommand() *cobra.Command {
	var codexHome, storeDir, mountPoint, nativeRoot string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "deactivate",
		Short: "Restore ordinary Codex session directories from the retained native tree",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			options, err := namespaceOptions(codexHome, mountPoint, nativeRoot)
			if err != nil {
				return err
			}
			if !apply {
				result, err := sessionns.Inspect(options)
				if err != nil {
					return err
				}
				return writeNamespaceResult(command, FSNamespaceResult{Result: result, DryRun: true}, jsonOutput)
			}
			states, err := vfs.DiscoverSessionStates(resolveFoldStore(options.Home, storeDir))
			if err != nil {
				return err
			}
			if len(states) != 0 {
				return errors.New("rollback all managed sessions before deactivating the namespace")
			}
			if err := mountHealthProbe(options.Mount); err == nil {
				return errors.New("stop the filesystem service before deactivating the namespace")
			}
			result, err := sessionns.Deactivate(options)
			if err != nil {
				return err
			}
			return writeNamespaceResult(command, FSNamespaceResult{Result: result}, jsonOutput)
		},
	}
	addNamespaceFlags(command, &codexHome, &mountPoint, &nativeRoot)
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&apply, "apply", false, "Remove canonical links and restore native directories")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSNamespaceRecoverCommand() *cobra.Command {
	var codexHome, mountPoint, nativeRoot string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "recover",
		Short: "Recover an interrupted namespace activation or deactivation",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			options, err := namespaceOptions(codexHome, mountPoint, nativeRoot)
			if err != nil {
				return err
			}
			var result sessionns.Result
			if apply {
				result, err = sessionns.Recover(options)
			} else {
				result, err = sessionns.Inspect(options)
			}
			if err != nil {
				return err
			}
			return writeNamespaceResult(command, FSNamespaceResult{Result: result, DryRun: !apply}, jsonOutput)
		},
	}
	addNamespaceFlags(command, &codexHome, &mountPoint, &nativeRoot)
	command.Flags().BoolVar(&apply, "apply", false, "Apply journal recovery")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func namespaceOptions(codexHome string, mountPoint string, nativeRoot string) (sessionns.Options, error) {
	home, err := codex.ResolveHome(codexHome)
	if err != nil {
		return sessionns.Options{}, err
	}
	mount := defaultMountPoint(home, mountPoint)
	if nativeRoot == "" {
		nativeRoot = filepath.Join(home, "fold-native")
	}
	if !filepath.IsAbs(nativeRoot) {
		return sessionns.Options{}, errors.New("native root must be absolute")
	}
	return sessionns.Options{Home: home, Mount: mount, NativeRoot: filepath.Clean(nativeRoot), MountProbe: mountHealthProbe}, nil
}

func addNamespaceFlags(command *cobra.Command, codexHome *string, mountPoint *string, nativeRoot *string) {
	command.Flags().StringVar(codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(mountPoint, "mount", "", "Canonical mount path; defaults to <codex-home>/fold-fs")
	command.Flags().StringVar(nativeRoot, "native-root", "", "Retained native tree; defaults to <codex-home>/fold-native")
}

func writeNamespaceResult(command *cobra.Command, result FSNamespaceResult, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(command, result)
	}
	_, err := fmt.Fprintf(command.OutOrStdout(), "active=%t recovered=%t dry_run=%t home=%s mount=%s native_root=%s\n", result.Active, result.Recovered, result.DryRun, result.Home, result.Mount, result.NativeRoot)
	return err
}
