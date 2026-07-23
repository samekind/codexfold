package cli

import (
	"fmt"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/contain"
	"github.com/spf13/cobra"
)

func newContainsCommand() *cobra.Command {
	var codexHome string
	var includeSessionMeta bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "contains <contained-session-id> <container-session-id>",
		Short: "Check exact contiguous JSONL record-sequence containment",
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
			result, err := contain.Check(command.Context(),
				contain.Input{ID: containedSession.ID, Path: containedSession.RolloutPath},
				contain.Input{ID: containerSession.ID, Path: containerSession.RolloutPath},
				contain.Options{IgnoreSessionMeta: !includeSessionMeta},
			)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			if !result.Contained {
				_, err = fmt.Fprintf(command.OutOrStdout(), "contained=false session=%s container=%s records=%d bytes=%s\n",
					result.ContainedID, result.ContainerID, result.ContainedRecords, formatBytes(result.ContainedBytes))
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(),
				"contained=true verified=%t session=%s container=%s records=%d bytes=%s record_range=%d..%d byte_range=%d..%d\n",
				result.VerifiedExact, result.ContainedID, result.ContainerID,
				result.ContainedRecords, formatBytes(result.ContainedBytes),
				result.ContainerStartRecord, result.ContainerEndRecord,
				result.ContainerStartByte, result.ContainerEndByte)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().BoolVar(&includeSessionMeta, "include-session-meta", false, "Require the first session_meta record to match exactly")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}
