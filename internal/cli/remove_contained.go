package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/prune"
	"github.com/spf13/cobra"
)

func newRemoveContainedCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var includeSessionMeta bool
	var apply bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "remove-contained <contained-session-id> <container-session-id>",
		Short: "Remove a verified archived session while retaining its recovery fold",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			sessions, err := codex.LoadSessions(home)
			if err != nil {
				return err
			}
			containedSession, err := findSession(sessions, args[0])
			if err != nil {
				return err
			}
			containerSession, err := findSession(sessions, args[1])
			if err != nil {
				return err
			}
			result, err := prune.RemoveContained(command.Context(), home, resolveFoldStore(home, storeDir), containedSession, containerSession, prune.Options{
				Apply: apply, IncludeSessionMeta: includeSessionMeta, WriterActive: removeContainedWriterActive,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(),
				"session=%s container=%s contained=%t fold_verified=%t unfold_verified=%t dry_run=%t removed=%t recovery=%s\n",
				result.ContainedSessionID, result.ContainerSessionID, result.Contained,
				result.FoldVerified, result.UnfoldVerified, result.DryRun, result.Removed,
				valueOrDash(result.TombstonePath))
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&includeSessionMeta, "include-session-meta", false, "Require the first session_meta record to match exactly")
	command.Flags().BoolVar(&apply, "apply", false, "Remove the verified archived session; omitted means proof-only dry-run")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	command.AddCommand(newRemoveContainedRecoverCommand())
	return command
}

func newRemoveContainedRecoverCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var apply bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "recover <contained-session-id>",
		Short: "Finish or roll back one interrupted contained-session removal",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if !apply {
				return errors.New("contained-session recovery requires --apply")
			}
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			result, err := prune.RecoverContained(command.Context(), home, resolveFoldStore(home, storeDir), args[0], prune.Options{
				Apply: true, WriterActive: removeContainedWriterActive,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s rolled_back=%t finalized=%t source=%s pending=%s\n",
				result.SessionID, result.RolledBack, result.Finalized, result.SourcePath, result.PendingPath)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&apply, "apply", false, "Apply deterministic contained-session removal recovery")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func removeContainedWriterActive(ctx context.Context, session codex.Session) (bool, error) {
	writers, err := enrollmentWriterProbe(ctx, []codex.Session{session})
	if err != nil {
		return false, fmt.Errorf("probe native session writers: %w", err)
	}
	return writers[session.ID], nil
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
