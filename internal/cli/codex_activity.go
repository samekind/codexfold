package cli

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// codexActivity reports whether Codex is currently running. The product may only
// be updated once Codex is completely closed, which means Desktop, CLI and
// app-server are all gone.
type codexActivity struct {
	Running  bool
	Unknown  bool
	Observed []string
}

// codexProcessLister returns one line per process, each holding an executable
// path or command. It is injected so the matching rules can be tested without
// depending on what happens to be running on the host.
type codexProcessLister func(context.Context) ([]string, error)

// codexActivityForUpdates is the seam the update commands use. Production always
// inspects the host; tests replace it so their expectations do not depend on what
// happens to be running on the machine running them.
var codexActivityForUpdates = func(ctx context.Context) codexActivity {
	return detectCodexActivity(ctx, nil)
}

func detectCodexActivity(ctx context.Context, list codexProcessLister) codexActivity {
	if list == nil {
		list = listHostProcesses
	}
	lines, err := list(ctx)
	if err != nil {
		return codexActivity{Unknown: true}
	}
	activity := codexActivity{}
	seen := make(map[string]struct{})
	for _, line := range lines {
		name := codexProcessKind(line)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		activity.Observed = append(activity.Observed, name)
		activity.Running = true
	}
	return activity
}

// codexProcessKind names the Codex component a process line belongs to, or an
// empty string. CodexFold's own processes must never match: its binary and its
// helpers all contain "codex", so matching is done on exact executable names and
// on the Desktop bundle path rather than on a substring of the command.
func codexProcessKind(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	executable := trimmed
	if fields := strings.Fields(trimmed); len(fields) > 0 {
		executable = fields[0]
	}
	base := filepath.Base(executable)
	if strings.HasPrefix(base, "codexfold") || strings.Contains(executable, "CodexFold") {
		return ""
	}
	switch base {
	case "codex":
		return "cli"
	case "codex-app-server", "codex_app_server":
		return "app-server"
	}
	if strings.Contains(executable, "/ChatGPT.app/") {
		return "desktop"
	}
	return ""
}

func listHostProcesses(ctx context.Context) ([]string, error) {
	command := exec.CommandContext(ctx, "ps", "-A", "-o", "args=")
	output, err := command.Output()
	if err != nil {
		return nil, errors.Join(errors.New("list host processes"), err)
	}
	return strings.Split(strings.TrimRight(string(output), "\n"), "\n"), nil
}
