package cli

import (
	"fmt"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/family"
	"github.com/spf13/cobra"
)

func newForkFamilyCommand() *cobra.Command {
	command := &cobra.Command{Use: "fork-family", Short: "Report fork graph and exact content evidence without mutation"}
	command.AddCommand(newForkFamilyShowCommand())
	command.AddCommand(newForkFamilyCompareCommand())
	return command
}

func newForkFamilyShowCommand() *cobra.Command {
	var codexHome string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "show <session-id>",
		Short: "List the spawn-edge family and active or archived state",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			sessions, err := codex.LoadSessions(home)
			if err != nil {
				return err
			}
			edges, err := codex.LoadSpawnEdges(home)
			if err != nil {
				return err
			}
			report, err := family.Build(args[0], sessions, edges)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, report)
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(), "seed=%s members=%d edges=%d missing=%d\n", report.SeedID, len(report.Members), len(report.Edges), len(report.MissingSessionIDs)); err != nil {
				return err
			}
			for _, member := range report.Members {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "session=%s relation=%s archived=%t path=%s\n", member.ID, member.RelationToSeed, member.Archived, member.RolloutPath); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newForkFamilyCompareCommand() *cobra.Command {
	var codexHome string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "compare <left-session-id> <right-session-id>",
		Short: "Compare two explicitly selected rollouts using exact record evidence",
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
			left, err := findSession(sessions, args[0])
			if err != nil {
				return err
			}
			right, err := findSession(sessions, args[1])
			if err != nil {
				return err
			}
			edges, err := codex.LoadSpawnEdges(home)
			if err != nil {
				return err
			}
			comparison, err := family.Compare(command.Context(), left, right, edges)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, comparison)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "left=%s right=%s graph=%s relation=%s exact=%t shared_prefix=%d shared=%d left_archived=%t right_archived=%t\n",
				comparison.LeftID, comparison.RightID, comparison.GraphRelation, comparison.Relation,
				comparison.VerifiedExact, comparison.SharedPrefixRecords, comparison.SharedRecords,
				comparison.LeftArchived, comparison.RightArchived)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}
