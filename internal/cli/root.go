package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/scan"
	"github.com/spf13/cobra"
)

var Version = "dev"

func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "codexfold",
		Short:         "Local-first Codex session storage analysis",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       resolvedVersion(),
	}
	root.AddCommand(newScanCommand())
	root.AddCommand(newContainsCommand())
	root.AddCommand(newRemoveContainedCommand())
	root.AddCommand(newFoldCommand())
	root.AddCommand(newUnfoldCommand("unfold"))
	root.AddCommand(newUnfoldCommand("materialize"))
	root.AddCommand(newDoctorCommand())
	root.AddCommand(newGCCommand())
	root.AddCommand(newPackCommand())
	root.AddCommand(newFSCommand())
	return root
}

func resolvedVersion() string {
	if Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return Version
}

func newScanCommand() *cobra.Command {
	var options scan.Options
	var codexHome string
	var layers string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "scan [session-id...]",
		Short: "Measure exact duplicate fields, records, and content-defined chunks",
		RunE: func(command *cobra.Command, args []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			sessions, err := codex.LoadSessions(home)
			if err != nil {
				return err
			}
			options.SessionIDs = args
			options.Layers = strings.Split(layers, ",")
			if !jsonOutput {
				options.Progress = func(completed int, total int, session codex.Session) {
					_, _ = fmt.Fprintf(command.ErrOrStderr(), "\rscan %d/%d %s", completed, total, session.ID)
					if completed == total {
						_, _ = fmt.Fprintln(command.ErrOrStderr())
					}
				}
			}
			result, err := scan.Evaluate(command.Context(), sessions, options)
			if err != nil {
				return err
			}
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(result)
			}
			return renderResult(command.OutOrStdout(), result)
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&options.Search, "search", "", "Select sessions matching title, id, or workspace path")
	command.Flags().BoolVar(&options.All, "all", false, "Scan all discovered sessions")
	command.Flags().BoolVar(&options.ExcludeArchived, "exclude-archived", false, "Exclude archived sessions")
	command.Flags().IntVar(&options.Limit, "limit", 0, "Maximum sessions after sorting largest first; 0 means unlimited")
	command.Flags().Int64Var(&options.MaxBytes, "max-bytes", 0, "Maximum selected source bytes; 0 means unlimited")
	command.Flags().StringVar(&options.IndexPath, "index", "", "SQLite index path; omitted uses and removes a temporary index")
	command.Flags().BoolVar(&options.OverwriteIndex, "overwrite-index", false, "Replace an existing disposable scan index")
	command.Flags().BoolVar(&options.Incremental, "incremental", false, "Reuse a persistent index, skipping unchanged files and scanning append-only tails")
	command.Flags().StringVar(&layers, "layers", scan.LayerField, "Comma-separated layers: field, record, cdc")
	command.Flags().Int64Var(&options.MinFieldBytes, "min-field-bytes", 4*1024, "Minimum raw JSON string token bytes to index")
	command.Flags().Int64Var(&options.MaxJSONLineBytes, "max-json-line-bytes", 32*1024*1024, "Maximum JSONL record bytes parsed for fields")
	command.Flags().Int64Var(&options.CDC.MinBytes, "cdc-min-bytes", 64*1024, "Minimum content-defined chunk bytes")
	command.Flags().Int64Var(&options.CDC.AverageBytes, "cdc-average-bytes", 256*1024, "Target content-defined chunk bytes; must be a power of two")
	command.Flags().Int64Var(&options.CDC.MaxBytes, "cdc-max-bytes", 1024*1024, "Maximum content-defined chunk bytes")
	command.Flags().IntVar(&options.Top, "top", 10, "Top repeated objects retained per layer")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func renderResult(writer io.Writer, result scan.Result) error {
	if _, err := fmt.Fprintf(writer,
		"sessions=%d corpus=%s processed=%s skipped=%d appended=%d duration=%s index=%s missing=%d changed=%d\n",
		result.SessionCount,
		formatBytes(result.Scan.ScannedBytes),
		formatBytes(result.ProcessedBytes),
		result.SkippedSessionCount,
		result.AppendedSessionCount,
		formatDuration(result.DurationMillis),
		formatBytes(result.IndexBytes),
		result.MissingSessionCount,
		result.ChangedSessionCount,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, ""); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "LAYER   OBJECTS    UNIQUE     DUPLICATES  DUPLICATE BYTES  CORPUS SHARE"); err != nil {
		return err
	}
	for _, layer := range result.Layers {
		share := float64(0)
		if result.Scan.ScannedBytes > 0 {
			share = float64(layer.DuplicateBytes) / float64(result.Scan.ScannedBytes) * 100
		}
		if _, err := fmt.Fprintf(writer, "%-7s %-10d %-10d %-11d %-16s %6.2f%%\n",
			layer.Layer,
			layer.ObjectCount,
			layer.UniqueObjectCount,
			layer.DuplicateOccurrences,
			formatBytes(layer.DuplicateBytes),
			share,
		); err != nil {
			return err
		}
	}
	return nil
}

func formatBytes(value int64) string {
	const unit = int64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	amount := float64(value)
	for _, name := range units {
		amount /= 1024
		if amount < 1024 || name == units[len(units)-1] {
			return fmt.Sprintf("%.2f %s", amount, name)
		}
	}
	return fmt.Sprintf("%d B", value)
}

func formatDuration(milliseconds int64) string {
	if milliseconds < 1000 {
		return fmt.Sprintf("%dms", milliseconds)
	}
	return fmt.Sprintf("%.2fs", float64(milliseconds)/1000)
}
