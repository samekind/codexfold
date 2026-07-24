package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/compat"
	"github.com/spf13/cobra"
)

type FSCompatibilityImportResult struct {
	Contract compat.Contract `json:"contract"`
	Path     string          `json:"path,omitempty"`
	DryRun   bool            `json:"dry_run"`
}

func newFSCompatibilityImportCommand() *cobra.Command {
	var codexHome, storeDir, tracePath, clientKind, clientVersion, platform string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "compatibility-import",
		Short: "Import sanitized client-specific regression evidence from a real filesystem trace",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !filepath.IsAbs(tracePath) || (clientKind != "cli" && clientKind != "desktop") || clientVersion == "" || platform == "" {
				return errors.New("absolute trace path, cli or desktop client kind, version, and platform are required")
			}
			trace, err := os.Open(tracePath)
			if err != nil {
				return err
			}
			contract, parseErr := compat.ParseFSUsage(trace, compat.ContractOptions{Platform: platform, ClientKind: clientKind, ClientVersion: clientVersion})
			closeErr := trace.Close()
			if parseErr != nil {
				return parseErr
			}
			if closeErr != nil {
				return closeErr
			}
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			result := FSCompatibilityImportResult{Contract: contract, DryRun: !apply}
			if apply {
				result.Path, err = compat.Save(filepath.Join(resolveFoldStore(home, storeDir), "compatibility"), contract)
				if err != nil {
					return err
				}
				result.DryRun = false
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "kind=%s version=%s operations=%d dry_run=%t path=%s\n", contract.ClientKind, contract.ClientVersion, len(contract.Operations), result.DryRun, result.Path)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&tracePath, "trace", "", "Absolute path to a real fs_usage-compatible trace")
	command.Flags().StringVar(&clientKind, "client-kind", "", "Client kind: cli or desktop")
	command.Flags().StringVar(&clientVersion, "client-version", "", "Exact client version represented by the trace")
	command.Flags().StringVar(&platform, "platform", runtime.GOOS, "Trace platform")
	command.Flags().BoolVar(&apply, "apply", false, "Persist the sanitized contract")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}
