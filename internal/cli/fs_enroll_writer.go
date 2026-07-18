package cli

import (
	"path/filepath"
	"strings"

	"github.com/jstar0/codexfold/internal/codex"
)

var enrollmentWriterProbe = detectEnrollmentWriters

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
