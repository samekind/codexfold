package cli

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/codex"
)

var enrollmentWriterProbe = detectEnrollmentWriters

// filesystemMigrationWriterProbe is kept injectable so migration tests can
// exercise the destructive boundary without depending on the host lsof view.
var filesystemMigrationWriterProbe = probeFilesystemMigrationWriter

func probeFilesystemMigrationWriter(ctx context.Context, session codex.Session, aliases ...string) (bool, error) {
	sessions := []codex.Session{session}
	for _, alias := range aliases {
		if alias != "" && filepath.Clean(alias) != filepath.Clean(session.RolloutPath) {
			sessions = append(sessions, codex.Session{ID: session.ID, RolloutPath: alias})
		}
	}
	writers, err := enrollmentWriterProbe(ctx, sessions)
	if err != nil {
		return false, err
	}
	return writers[session.ID], nil
}

func parseEnrollmentWriterSnapshot(output []byte, sessions []codex.Session) map[string]bool {
	aliases := make(map[string][]string, len(sessions)*2)
	for _, session := range sessions {
		for _, path := range enrollmentPathAliases(session.RolloutPath) {
			aliases[path] = append(aliases[path], session.ID)
		}
	}
	writers := make(map[string]bool)
	var access string
	var name string
	flush := func() {
		if name == "" || !strings.ContainsAny(access, "wu") {
			access = ""
			name = ""
			return
		}
		for _, sessionID := range aliases[canonicalEnrollmentPath(name)] {
			writers[sessionID] = true
		}
		access = ""
		name = ""
	}
	for _, line := range strings.Split(string(output), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p', 'f':
			flush()
		case 'a':
			access = line[1:]
		case 'n':
			name = strings.TrimSuffix(line[1:], " (deleted)")
		}
	}
	flush()
	return writers
}

func enrollmentPathAliases(path string) []string {
	path = canonicalEnrollmentPath(path)
	aliases := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		aliases = append(aliases, resolved)
	}
	return aliases
}

func canonicalEnrollmentPath(path string) string {
	return filepath.Clean(path)
}
