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
	command.AddCommand(newPackRetireLooseCommand())
	return command
}

func newPackRetireLooseCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var apply bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "retire-loose",
		Short: "Retire loose objects only after pack-only recovery verification",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			result, err := pack.RetireLoose(command.Context(), resolveFoldStore(home, storeDir), pack.RetireLooseOptions{Apply: apply})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "generation=%s dry_run=%t candidates=%d candidate_bytes=%s retired=%d retired_bytes=%s actual_reclaimed=%s\n", result.Generation, result.DryRun, result.CandidateCount, formatBytes(result.CandidateBytes), result.RetiredCount, formatBytes(result.RetiredBytes), formatBytes(result.ActualReclaimedBytes))
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&apply, "apply", false, "Delete eligible loose objects after pack-only verification")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
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
		Short: "Verify packed objects and complete manifest reconstruction",
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
			_, err = fmt.Fprintf(command.OutOrStdout(), "generation=%s objects=%d verified=%d manifests=%d verified_manifests=%d issues=%d\n", result.Generation, result.ObjectCount, result.VerifiedCount, result.ManifestCount, result.VerifiedManifestCount, result.IssueCount)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}
