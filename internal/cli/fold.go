package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/scan"
	"github.com/spf13/cobra"
)

func newFoldCommand() *cobra.Command {
	var codexHome string
	var jsonOutput bool
	var recordIndexPath string
	var options fold.FoldOptions
	command := &cobra.Command{
		Use:   "fold <session-id>",
		Short: "Create a verified content-addressed fold; dry-run by default",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			options.StoreDir = resolveFoldStore(home, options.StoreDir)
			sessions, err := codex.LoadSessions(home)
			if err != nil {
				return err
			}
			session, err := findSession(sessions, args[0])
			if err != nil {
				return err
			}
			resolver, packErr := pack.Open(options.StoreDir, pack.OpenOptions{CacheBytes: -1})
			if packErr == nil {
				defer resolver.Close()
				options.ExistingReader = resolver
			}
			if recordIndexPath != "" {
				recordIndex, err := scan.OpenDuplicateRecordIndex(recordIndexPath)
				if err != nil {
					return err
				}
				defer recordIndex.Close()
				options.RecordIndex = recordIndex
			}
			result, err := fold.Fold(command.Context(), toFoldSession(session), options)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s verified=%t dry_run=%t parts=%d records=%d fields=%d residual=%d new=%s removed=%t manifest=%s\n",
				result.SessionID, result.Verified, result.DryRun, result.PartCount,
				result.RecordParts, result.FieldParts, result.ResidualParts, formatBytes(result.NewStoredBytes),
				result.RemovedSource, result.ManifestPath)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&options.StoreDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&options.Apply, "apply", false, "Write objects and manifest; omitted means dry-run")
	command.Flags().BoolVar(&options.Overwrite, "overwrite", false, "Replace an existing manifest after verification")
	command.Flags().BoolVar(&options.RemoveSource, "remove-source", false, "Remove the source rollout after stored reconstruction verification")
	command.Flags().BoolVar(&options.AllowActive, "allow-active", false, "Allow source removal for a non-archived session")
	command.Flags().Int64Var(&options.FieldThreshold, "field-threshold", 4*1024, "Minimum exact raw JSON string token bytes extracted as a field object")
	command.Flags().Int64Var(&options.MaxJSONLineBytes, "max-json-line-bytes", 32*1024*1024, "Maximum JSONL line bytes parsed for field extraction")
	command.Flags().Int64Var(&options.CDC.MinBytes, "cdc-min-bytes", 4*1024, "Minimum residual content-defined chunk bytes")
	command.Flags().Int64Var(&options.CDC.AverageBytes, "cdc-average-bytes", 16*1024, "Target residual chunk bytes; must be a power of two")
	command.Flags().Int64Var(&options.CDC.MaxBytes, "cdc-max-bytes", 64*1024, "Maximum residual content-defined chunk bytes")
	command.Flags().StringVar(&recordIndexPath, "record-index", "", "Explicit record-layer scan index for conservative Fold V2")
	command.Flags().Int64Var(&options.RecordThreshold, "record-threshold", 4*1024, "Minimum bytes for promoting a confirmed duplicate record")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newUnfoldCommand(name string) *cobra.Command {
	var codexHome string
	var storeDir string
	var targetPath string
	var overwrite bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   name + " <session-id>",
		Short: "Restore a fold and verify the original SHA-256",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			storeDir = resolveFoldStore(home, storeDir)
			var reader fold.ObjectReader
			resolver, packErr := pack.Open(storeDir, pack.OpenOptions{})
			if packErr == nil {
				defer resolver.Close()
				reader = resolver
			}
			result, err := fold.UnfoldWithOptions(command.Context(), storeDir, args[0], fold.UnfoldOptions{TargetPath: targetPath, Overwrite: overwrite, Reader: reader})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "session=%s verified=%t bytes=%s target=%s\n", result.SessionID, result.Verified, formatBytes(result.Bytes), result.TargetPath)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&targetPath, "to", "", "Restore target; defaults to the original rollout path")
	command.Flags().BoolVar(&overwrite, "overwrite", false, "Replace an existing restore target after verification")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newDoctorCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Verify manifests, objects, and complete fold reconstruction",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			storeDir = resolveFoldStore(home, storeDir)
			var reader fold.ObjectReader
			resolver, packErr := pack.Open(storeDir, pack.OpenOptions{CacheBytes: -1})
			if packErr == nil {
				defer resolver.Close()
				reader = resolver
			}
			result, err := fold.DoctorWithOptions(command.Context(), storeDir, fold.DoctorOptions{Reader: reader})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "manifests=%d verified=%d objects=%d issues=%d store=%s\n", result.ManifestCount, result.VerifiedManifestCount, result.UniqueObjectCount, result.IssueCount, result.StoreDir)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newGCCommand() *cobra.Command {
	var codexHome string
	var storeDir string
	var apply bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "gc",
		Short: "Find unreferenced objects; dry-run by default",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			storeDir = resolveFoldStore(home, storeDir)
			result, err := fold.GC(command.Context(), storeDir, apply)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t referenced=%d orphans=%d orphan_bytes=%s removed=%d removed_bytes=%s\n", result.DryRun, result.Referenced, result.OrphanCount, formatBytes(result.OrphanBytes), result.RemovedCount, formatBytes(result.RemovedBytes))
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().BoolVar(&apply, "apply", false, "Remove unreferenced objects")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func resolveFoldStore(codexHome string, explicit string) string {
	if explicit != "" {
		return filepath.Clean(explicit)
	}
	return filepath.Join(codexHome, "fold-store")
}

func findSession(sessions []codex.Session, sessionID string) (codex.Session, error) {
	for _, session := range sessions {
		if session.ID == sessionID {
			return session, nil
		}
	}
	return codex.Session{}, fmt.Errorf("Codex session not found: %s", sessionID)
}

func toFoldSession(session codex.Session) fold.Session {
	return fold.Session{
		ID: session.ID, Title: session.Title, CWD: session.CWD,
		RolloutPath: session.RolloutPath, Archived: session.Archived,
	}
}

func writeJSON(command *cobra.Command, value any) error {
	encoder := json.NewEncoder(command.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
