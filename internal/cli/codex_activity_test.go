package cli

import (
	"context"
	"errors"
	"testing"
)

// CodexFold's own processes all contain "codex", so a substring match would make
// the service permanently believe Codex is running and block every update.
// withCodexClosed pins the update commands' view of Codex for one test, so an
// expectation about update policy does not depend on whether the machine running
// the suite happens to have Codex open.
func withCodexClosed(t *testing.T) {
	t.Helper()
	previous := codexActivityForUpdates
	codexActivityForUpdates = func(context.Context) codexActivity { return codexActivity{} }
	t.Cleanup(func() { codexActivityForUpdates = previous })
}

func TestCodexProcessKindNeverMatchesCodexFoldItself(t *testing.T) {
	for _, line := range []string{
		"/Users/x/Library/Application Support/CodexFold/acceptance-a1/codexfold-candidate fs serve --apply",
		"/usr/local/bin/codexfold fs supervise --apply",
		"/Users/x/Applications/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit --run-helper /usr/local/bin/codexfold fs serve",
		"/Users/x/Applications/CodexFoldFSKit.app/Contents/MacOS/CodexFoldIncidentMonitor",
	} {
		if kind := codexProcessKind(line); kind != "" {
			t.Fatalf("CodexFold process classified as Codex %q: %s", kind, line)
		}
	}
}

func TestCodexProcessKindRecognisesEveryCodexComponent(t *testing.T) {
	for _, testCase := range []struct {
		line string
		want string
	}{
		{line: "/opt/homebrew/bin/codex exec --skip-git-repo-check hello", want: "cli"},
		{line: "/opt/homebrew/bin/codex", want: "cli"},
		{line: "/opt/homebrew/lib/codex-app-server --port 0", want: "app-server"},
		{line: "/Applications/ChatGPT.app/Contents/MacOS/ChatGPT", want: "desktop"},
		{line: "/Applications/ChatGPT.app/Contents/Frameworks/helper", want: "desktop"},
		{line: "/usr/bin/vim notes.txt", want: ""},
		{line: "   ", want: ""},
	} {
		if kind := codexProcessKind(testCase.line); kind != testCase.want {
			t.Fatalf("kind(%q) = %q, want %q", testCase.line, kind, testCase.want)
		}
	}
}

func TestDetectCodexActivityReportsRunningComponentsOnce(t *testing.T) {
	activity := detectCodexActivity(context.Background(), func(context.Context) ([]string, error) {
		return []string{
			"/opt/homebrew/bin/codex exec one",
			"/opt/homebrew/bin/codex exec two",
			"/Applications/ChatGPT.app/Contents/MacOS/ChatGPT",
			"/usr/local/bin/codexfold fs serve",
		}, nil
	})
	if !activity.Running || activity.Unknown {
		t.Fatalf("activity = %#v, want running and known", activity)
	}
	if len(activity.Observed) != 2 {
		t.Fatalf("observed = %v, want exactly cli and desktop", activity.Observed)
	}
}

// Failing to look is not proof that Codex is closed.
func TestDetectCodexActivityIsUnknownWhenListingFails(t *testing.T) {
	activity := detectCodexActivity(context.Background(), func(context.Context) ([]string, error) {
		return nil, errors.New("ps unavailable")
	})
	if !activity.Unknown || activity.Running {
		t.Fatalf("activity = %#v, want unknown", activity)
	}
}

func TestDetectCodexActivityIsQuietWhenCodexIsClosed(t *testing.T) {
	activity := detectCodexActivity(context.Background(), func(context.Context) ([]string, error) {
		return []string{"/usr/local/bin/codexfold fs serve", "/usr/bin/vim"}, nil
	})
	if activity.Running || activity.Unknown {
		t.Fatalf("activity = %#v, want closed and known", activity)
	}
}
