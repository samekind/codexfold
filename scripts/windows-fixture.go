//go:build ignore

// Creates synthetic sessions for scripts/test-windows-use.ps1. Never reads the real Codex home.
package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	if err := create(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func create() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: go run scripts/windows-fixture.go <new-test-root>")
	}
	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		return err
	}
	// Refuse every existing directory: a fixture can never overwrite user data.
	if err := os.Mkdir(root, 0700); err != nil {
		return err
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0700); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`create table threads (id text primary key, title text, cwd text, rollout_path text, model_provider text, model text, updated_at integer, archived integer, git_branch text)`)
	if err != nil {
		return err
	}
	type session struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
	}
	var sessions []session
	for i, id := range []string{"a1111111-1111-4111-8111-111111111111", "b2222222-2222-4222-8222-222222222222"} {
		var content bytes.Buffer
		encoder := json.NewEncoder(&content)
		if err := encoder.Encode(map[string]any{"timestamp": "2026-10-01T00:00:00Z", "type": "session_meta", "payload": map[string]any{"id": id, "cwd": root, "originator": "codexfold-windows-test", "cli_version": "fixture"}}); err != nil {
			return err
		}
		for record := 0; record < 192; record++ {
			payload := map[string]any{"type": "agent_message", "message": strings.Repeat("CodexFold Windows byte-preserving test. 中文会话内容。\n", 180)}
			if err := encoder.Encode(map[string]any{"timestamp": "2026-10-01T00:00:01Z", "type": "event_msg", "payload": payload}); err != nil {
				return err
			}
		}
		if err := encoder.Encode(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "agent_message", "message": fmt.Sprintf("Independent session %d", i)}}); err != nil {
			return err
		}
		rollout := filepath.Join(home, "sessions", "rollout-2026-10-01T00-00-00-"+id+".jsonl")
		if err := os.WriteFile(rollout, content.Bytes(), 0600); err != nil {
			return err
		}
		digest := sha256.Sum256(content.Bytes())
		if _, err := db.Exec(`insert into threads values (?, ?, ?, ?, 'fixture', 'fixture', ?, 0, '')`, id, "Windows isolated test", root, rollout, time.Now().Add(-2*time.Hour).Unix()); err != nil {
			return err
		}
		sessions = append(sessions, session{id, rollout, hex.EncodeToString(digest[:]), content.Len()})
	}
	if err := db.Close(); err != nil {
		return err
	}
	metadata := map[string]any{"root": root, "home": home, "store": filepath.Join(root, "store"), "mount": filepath.Join(root, "mount"), "sessions": sessions}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), encoded, 0600); err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}
