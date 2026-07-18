package cli

import (
	"context"
	"errors"
	"fmt"

	archivepkg "github.com/jstar0/codexfold/internal/archive"
	"github.com/jstar0/codexfold/internal/codex"
	"github.com/spf13/cobra"
)

type archiveFlags struct {
	codexHome string
	storeDir  string
	apply     bool
	json      bool
}

func newArchiveCommand() *cobra.Command {
	var flags archiveFlags
	command := &cobra.Command{
		Use:   "archive <session-id>",
		Short: "Preview or explicitly archive one selected Codex session",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(flags.codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, flags.storeDir)
			sessions, err := codex.LoadSessions(home)
			if err != nil {
				return err
			}
			session, err := findSession(sessions, args[0])
			if err != nil {
				return err
			}
			writers, err := probeArchiveWriters(command.Context(), sessions)
			if err != nil {
				return err
			}
			result, err := archivepkg.Archive(command.Context(), home, store, session, archivepkg.Options{
				Apply: flags.apply,
				WriterActive: func(_ context.Context, selected codex.Session) (bool, error) {
					return writers[selected.ID], nil
				},
			})
			if err != nil {
				return err
			}
			if flags.json {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t archived=%t session=%s bytes=%s sha256=%s target=%s\n",
				result.DryRun, result.Archived, result.SessionID, formatBytes(result.Bytes), result.SHA256, result.TargetPath)
			return err
		},
	}
	addArchiveFlags(command, &flags)
	command.AddCommand(newArchiveRecoverCommand())
	return command
}

func newArchiveRecoverCommand() *cobra.Command {
	var flags archiveFlags
	command := &cobra.Command{
		Use:   "recover <session-id>",
		Short: "Recover one interrupted archive transaction",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if !flags.apply {
				return errors.New("archive recovery requires --apply")
			}
			home, err := codex.ResolveHome(flags.codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, flags.storeDir)
			sessions, err := codex.LoadSessions(home)
			if err != nil {
				return err
			}
			writers, err := probeArchiveWriters(command.Context(), sessions)
			if err != nil {
				return err
			}
			if writers[args[0]] {
				return errors.New("cannot recover an archive transaction while the selected session has an active writer")
			}
			result, err := archivepkg.Recover(command.Context(), home, store, args[0])
			if err != nil {
				return err
			}
			if flags.json {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "rolled_back=%t finalized=%t session=%s source=%s target=%s\n",
				result.RolledBack, result.Finalized, result.SessionID, result.SourcePath, result.TargetPath)
			return err
		},
	}
	addArchiveFlags(command, &flags)
	return command
}

func addArchiveFlags(command *cobra.Command, flags *archiveFlags) {
	command.Flags().StringVar(&flags.codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&flags.storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&flags.apply, "apply", false, "Apply the explicit archive or recovery mutation")
	command.Flags().BoolVar(&flags.json, "json", false, "Emit JSON output")
}

func probeArchiveWriters(ctx context.Context, sessions []codex.Session) (map[string]bool, error) {
	writers, err := enrollmentWriterProbe(ctx, sessions)
	if err != nil {
		return nil, fmt.Errorf("probe native session writers: %w", err)
	}
	return writers, nil
}
