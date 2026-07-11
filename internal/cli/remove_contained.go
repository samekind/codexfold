package cli

import (
	"fmt"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/prune"
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
				Apply: apply, IncludeSessionMeta: includeSessionMeta,
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
	return command
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
