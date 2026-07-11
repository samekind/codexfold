package codex

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type Session struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	CWD           string `json:"cwd"`
	RolloutPath   string `json:"rollout_path"`
	ModelProvider string `json:"model_provider"`
	Model         string `json:"model"`
	UpdatedAt     int64  `json:"updated_at"`
	Archived      bool   `json:"archived"`
	GitBranch     string `json:"git_branch,omitempty"`
}

func ResolveHome(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return filepath.Clean(explicit), nil
	}
	if value := strings.TrimSpace(os.Getenv("CODEX_HOME")); value != "" {
		return filepath.Clean(value), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func LoadSessions(home string) ([]Session, error) {
	dbPath := filepath.Join(home, "state_5.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("locate Codex state database %s: %w", dbPath, err)
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     dbPath,
		RawQuery: "mode=ro&_pragma=busy_timeout%285000%29",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open Codex state database: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`
		select id, title, cwd, rollout_path, model_provider, coalesce(model, ''),
		       updated_at, archived, coalesce(git_branch, '')
		from threads
		order by updated_at desc
	`)
	if err != nil {
		return nil, fmt.Errorf("query Codex sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sessions := make([]Session, 0)
	for rows.Next() {
		var session Session
		var archived int64
		if err := rows.Scan(
			&session.ID,
			&session.Title,
			&session.CWD,
			&session.RolloutPath,
			&session.ModelProvider,
			&session.Model,
			&session.UpdatedAt,
			&archived,
			&session.GitBranch,
		); err != nil {
			return nil, fmt.Errorf("scan Codex session: %w", err)
		}
		session.Archived = archived != 0
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Codex sessions: %w", err)
	}
	return sessions, nil
}
