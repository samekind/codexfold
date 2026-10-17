//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/mountid"
	"github.com/samekind/codexfold/internal/service"
	"github.com/spf13/cobra"
	"os"
)

func addWindowsLiveDaemonUpdateCommand(parent *cobra.Command) {
	parent.AddCommand(newWindowsCoreUpdateCommand())
	parent.AddCommand(&cobra.Command{Use: "core-protocol", Hidden: true, Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return writeJSON(command, struct {
				Version int `json:"version"`
			}{mountfs.WindowsCoreProtocol})
		}})
}

func newWindowsCoreUpdateCommand() *cobra.Command {
	var definitionPath string
	var apply, jsonOutput bool
	command := &cobra.Command{Use: "update-daemon-live <candidate>", Short: "Replace the Windows storage engine while preserving its resident WinFsp mount", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			candidate, err := filepath.Abs(arguments[0])
			if err != nil {
				return err
			}
			definition, err := resolveServiceDefinitionPath(definitionPath)
			if err != nil {
				return err
			}
			store, err := service.DefinitionStore(service.PlatformWindows, definition)
			if err != nil {
				return err
			}
			mount, err := service.DefinitionMountPoint(service.PlatformWindows, definition)
			if err != nil {
				return err
			}
			probe, cancelProbe := context.WithTimeout(command.Context(), 5*time.Second)
			defer cancelProbe()
			executable := exec.CommandContext(probe, candidate, "fs", "service", "core-protocol")
			configureEnrollmentChild(executable)
			output, err := executable.Output()
			var protocol struct {
				Version int `json:"version"`
			}
			if err != nil || json.Unmarshal(output, &protocol) != nil || protocol.Version != mountfs.WindowsCoreProtocol {
				return errors.New("candidate does not support the Windows engine protocol")
			}
			before, err := os.ReadFile(filepath.Join(mount, mountid.Path))
			if err != nil {
				return err
			}
			identity, err := mountid.Parse(before)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(command.Context(), 4*time.Minute)
			defer cancel()
			var result mountfs.CoreUpdateResult
			if err := mountfs.CallWindowsCoreControl(ctx, store, "Update", mountfs.CoreUpdateRequest{Candidate: candidate, Apply: apply}, &result); err != nil {
				return err
			}
			if apply {
				after, err := os.ReadFile(filepath.Join(mount, mountid.Path))
				if err != nil {
					return err
				}
				current, err := mountid.Parse(after)
				if err != nil {
					return err
				}
				if current.Nonce != identity.Nonce || current.BuildSHA256 != result.CandidateSHA256 {
					return errors.New("engine switched but the mounted identity could not be verified")
				}
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t changed=%t old_engine=%d new_engine=%d mount_preserved=%t build=%s recovery=%s\n", result.DryRun, result.Changed, result.PreviousPID, result.ReplacementPID, result.MountPreserved, result.Build, result.RecoveryBinary)
			return err
		}}
	addServiceDefinitionFlags(command, &definitionPath)
	command.Flags().BoolVar(&apply, "apply", false, "Replace the quiescent engine without stopping the mount or clients")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}
