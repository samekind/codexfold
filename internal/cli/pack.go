package cli

import (
	"fmt"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/spf13/cobra"
)

func newPackCommand() *cobra.Command {
	command := &cobra.Command{Use: "pack", Short: "Build and verify packed object generations"}
	command.AddCommand(newPackBuildCommand())
	command.AddCommand(newPackDoctorCommand())
	return command
}

func newPackBuildCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var options pack.BuildOptions
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "build",
		Short: "Build a verified immutable pack generation",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			result, err := pack.Build(command.Context(), resolveFoldStore(home, storeDir), options)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "generation=%s objects=%d blocks=%d packs=%d raw=%s stored=%s\n", result.Generation, result.ObjectCount, result.BlockCount, result.PackCount, formatBytes(result.RawBytes), formatBytes(result.StoredBytes))
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().Int64Var(&options.BlockBytes, "block-bytes", 0, "Uncompressed bytes per independently compressed block")
	command.Flags().Int64Var(&options.PackBytes, "pack-bytes", 0, "Maximum stored bytes per pack file")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newPackDoctorCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Verify the active packed object generation",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			result, err := pack.Doctor(command.Context(), resolveFoldStore(home, storeDir))
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "generation=%s objects=%d verified=%d issues=%d\n", result.Generation, result.ObjectCount, result.VerifiedCount, result.IssueCount)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}
