//go:build windows

package main

//go:generate go run assets/generate.go
//go:generate go run github.com/akavel/rsrc@v0.10.2 -manifest assets/app.manifest -ico assets/codexfold.ico,assets/codexfold-attention.ico -arch amd64 -o rsrc_windows_amd64.syso
//go:generate go run github.com/akavel/rsrc@v0.10.2 -manifest assets/app.manifest -ico assets/codexfold.ico,assets/codexfold-attention.ico -arch arm64 -o rsrc_windows_arm64.syso

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/tray"
)

func main() {
	store := flag.String("store", "", "CodexFold store directory; defaults to CODEX_HOME/fold-store")
	background := flag.Bool("background", false, "Start in the notification area")
	quit := flag.Bool("quit", false, "Close the tray for this store without stopping the filesystem")
	diagnostics := flag.String("diagnostics", "", "Export aggregate diagnostics to a JSON file and exit")
	flag.Parse()
	if *store == "" {
		home, err := codex.ResolveHome("")
		if err != nil {
			tray.ShowError(err)
			os.Exit(1)
		}
		*store = filepath.Join(home, "fold-store")
	}
	absolute, err := filepath.Abs(*store)
	if err == nil && *diagnostics != "" {
		output, outputErr := filepath.Abs(*diagnostics)
		if outputErr == nil {
			monitor := tray.NewMonitor(absolute)
			outputErr = tray.ExportDiagnostics(output, monitor.DiagnosticSnapshot(time.Now()))
		}
		if outputErr != nil {
			fmt.Fprintln(os.Stderr, outputErr)
			os.Exit(1)
		}
		return
	}
	if err == nil && *quit {
		if err := tray.RequestExit(absolute); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err == nil {
		err = tray.Run(absolute, *background)
	}
	if err != nil {
		tray.ShowError(fmt.Errorf("CodexFold: %w", err))
		os.Exit(1)
	}
}
