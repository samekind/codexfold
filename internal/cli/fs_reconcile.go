package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/samekind/codexfold/internal/reconcile"
	"github.com/spf13/cobra"
)

type FSReconcileRolloutResult struct {
	reconcile.Result
	DryRun bool `json:"dry_run"`
}

func newFSRepairRolloutCommand() *cobra.Command {
	var outputPath, orphanPath string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "repair-rollout <source.jsonl>",
		Short: "Recover deterministically interleaved JSONL writes into a separate rollout",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if !apply {
				return errors.New("repair-rollout requires --apply and writes only to a separate --output")
			}
			if !filepath.IsAbs(args[0]) || !filepath.IsAbs(outputPath) {
				return errors.New("source and --output paths must be absolute")
			}
			result, err := reconcile.RepairWithOptions(args[0], outputPath, reconcile.RepairOptions{AllowOrphans: orphanPath != "", OrphanPath: orphanPath, Context: command.Context()})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(),
				"physical=%d invalid=%d reconstructed=%d conversations=%d preserved=%d reconstructed_conversations=%d conversation_verified=%t orphans=%d output=%d regressions=%d max_buffer=%d path=%s sha256=%s\n",
				result.PhysicalLines,
				result.InvalidPhysicalLines,
				result.ReconstructedRecords,
				result.SourceConversationRecords,
				result.PreservedConversationRecords,
				result.ReconstructedConversationRecords,
				result.ConversationIntegrityVerified,
				result.OrphanLines,
				result.OutputRecords,
				result.TimestampRegressions,
				result.MaximumBufferedBytes,
				result.OutputPath,
				result.OutputSHA256,
			)
			return err
		},
	}
	command.Flags().StringVar(&outputPath, "output", "", "Absolute output path for the repaired rollout")
	command.Flags().StringVar(&orphanPath, "orphans", "", "Optional absolute path for unrecoverable raw fragments; enables salvage mode")
	command.Flags().BoolVar(&apply, "apply", false, "Write a separately verified repaired rollout")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSReconcileRolloutCommand() *cobra.Command {
	var outputPath string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "reconcile-rollout <base.jsonl> <branch.jsonl>",
		Short: "Reconcile two monotonic rollout branches without replacing either source",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if !filepath.IsAbs(args[0]) || !filepath.IsAbs(args[1]) {
				return errors.New("base and branch paths must be absolute")
			}
			var result reconcile.Result
			var err error
			if apply {
				if !filepath.IsAbs(outputPath) {
					return errors.New("--output must be absolute with --apply")
				}
				result, err = reconcile.MergeWithOptions(args[0], args[1], outputPath, reconcile.MergeOptions{Context: command.Context()})
			} else {
				result, err = reconcile.Analyze(args[0], args[1])
			}
			if err != nil {
				return err
			}
			wrapped := FSReconcileRolloutResult{Result: result, DryRun: !apply}
			if jsonOutput {
				return writeJSON(command, wrapped)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(),
				"base=%d branch=%d shared=%d base_only=%d added=%d output=%d regressions=%d/%d/%d dry_run=%t path=%s sha256=%s\n",
				result.Base.Records,
				result.Branch.Records,
				result.SharedRecords,
				result.BaseOnlyRecords,
				result.AddedFromBranch,
				result.OutputRecords,
				result.Base.TimestampRegressions,
				result.Branch.TimestampRegressions,
				result.OutputRegressions,
				wrapped.DryRun,
				result.OutputPath,
				result.OutputSHA256,
			)
			return err
		},
	}
	command.Flags().StringVar(&outputPath, "output", "", "Absolute output path for the reconciled rollout")
	command.Flags().BoolVar(&apply, "apply", false, "Write a separately verified reconciled rollout")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}
