package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/spf13/cobra"
)

func newFSEnrollReclaimCommand() *cobra.Command {
	var home, store string
	var apply, jsonOutput bool
	var workers int
	command := &cobra.Command{Use: "reclaim", Short: "Finish native, loose-object and old-pack cleanup without folding another batch", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		if !apply {
			return errors.New("reclaim requires --apply")
		}
		if workers < 0 || workers > 16 {
			return errors.New("workers must be between 0 and 16")
		}
		resolved, err := codex.ResolveHome(home)
		if err != nil {
			return err
		}
		ctx := context.WithValue(command.Context(), nativeRetirementWorkersKey{}, workers)
		result, err := runEnrollmentMaintenance(ctx, resolved, resolveFoldStore(resolved, store), 0, true)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(command, result)
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "native_retired=%d native_deferred=%d proof_passes=%d loose_retired=%t gc_removed=%d\n", result.NativeRetired, result.NativeDeferred, result.NativeProofPasses, result.LooseRetirementRan, result.StorageGC.RemovedCount)
		return err
	}}
	command.Flags().StringVar(&home, "codex-home", "", "Codex home")
	command.Flags().StringVar(&store, "store", "", "Fold store")
	command.Flags().BoolVar(&apply, "apply", false, "Apply verified cleanup")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON")
	command.Flags().IntVar(&workers, "workers", 0, "Native cleanup workers (0 or 1: serial; 2-16: explicit concurrency)")
	return command
}
