package tray

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"time"
)

type DiagnosticReport struct {
	Version        int
	GeneratedAt    string
	Platform       string
	GoVersion      string
	SourceRevision string
	SourceModified bool
	SessionContent bool
	Snapshot       View
}

func Diagnostics(view View) DiagnosticReport {
	report := DiagnosticReport{Version: 1, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Platform: runtime.GOOS + "/" + runtime.GOARCH, GoVersion: runtime.Version(), Snapshot: view}
	// An operation acknowledgement is transient UI state, not a diagnostic.
	report.Snapshot.Operation = nil
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				report.SourceRevision = setting.Value
			case "vcs.modified":
				report.SourceModified = setting.Value == "true"
			}
		}
	}
	return report
}

func DiagnosticBytes(view View) ([]byte, error) {
	return json.MarshalIndent(Diagnostics(view), "", "  ")
}
func ExportDiagnostics(path string, view View) error { return writeJSONFile(path, Diagnostics(view)) }
