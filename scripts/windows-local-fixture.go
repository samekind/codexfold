//go:build ignore

// Copies stable local sessions into an independent test home. Source access is
// read-only; no credentials, configuration, or live SQLite database are copied.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codex"
)

type localCandidate struct {
	session codex.Session
	info    os.FileInfo
}
type copiedSession struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	SourcePath string `json:"source_path"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	sourceHome := flag.String("source-home", "", "Local Codex home, opened read-only")
	rootArg := flag.String("root", "", "New test directory; omitted reports only inventory")
	flag.Parse()
	realHome, err := codex.ResolveHome(*sourceHome)
	if err != nil {
		return err
	}
	realHome, err = filepath.Abs(realHome)
	if err != nil {
		return err
	}
	sessions, err := codex.LoadSessions(realHome)
	if err != nil {
		return err
	}
	var candidates []localCandidate
	var present, missing, excluded int
	var totalBytes int64
	cutoff := time.Now().Add(-time.Hour)
	for _, s := range sessions {
		relative, err := filepath.Rel(realHome, s.RolloutPath)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			excluded++
			continue
		}
		info, err := os.Lstat(s.RolloutPath)
		if os.IsNotExist(err) {
			missing++
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			excluded++
			continue
		}
		present++
		totalBytes += info.Size()
		if info.Size() > 0 && info.Size() <= 64<<20 && info.ModTime().Before(cutoff) && s.UpdatedAt < cutoff.Unix() && filepath.Base(s.ID) == s.ID && !strings.ContainsAny(s.ID, "/\\\x00") {
			candidates = append(candidates, localCandidate{s, info})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].info.Size() > candidates[j].info.Size() })
	inventory := map[string]any{"source_home": realHome, "database_sessions": len(sessions), "present_files": present, "missing_files": missing, "excluded_files": excluded, "source_bytes": totalBytes, "stable_candidates": len(candidates)}
	if *rootArg == "" {
		return printJSON(inventory)
	}
	if len(candidates) < 2 {
		return errors.New("at least two stable local sessions are required")
	}
	root, err := filepath.Abs(*rootArg)
	if err != nil {
		return err
	}
	if root == realHome || strings.HasPrefix(strings.ToLower(root), strings.ToLower(realHome)+string(filepath.Separator)) {
		return errors.New("test directory must be outside the real Codex home")
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.Exec(`create table threads (id text primary key, title text, cwd text, rollout_path text, model_provider text, model text, updated_at integer, archived integer, git_branch text)`); err != nil {
		return err
	}
	var copies []copiedSession
	for _, candidate := range candidates {
		if len(copies) == 2 {
			break
		}
		s := candidate.session
		target := filepath.Join(home, "sessions", "rollout-local-"+s.ID+".jsonl")
		digest, err := copyStable(candidate, target)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Skipped changed/unavailable source:", s.ID, err)
			continue
		}
		if _, err = db.Exec(`insert into threads values (?, ?, ?, ?, ?, ?, ?, 0, '')`, s.ID, "Local session copy", root, target, s.ModelProvider, s.Model, s.UpdatedAt); err != nil {
			return err
		}
		copies = append(copies, copiedSession{s.ID, target, s.RolloutPath, digest, candidate.info.Size()})
	}
	if len(copies) != 2 {
		return errors.New("could not capture two stable local sessions")
	}
	if err := db.Close(); err != nil {
		return err
	}
	metadata := map[string]any{"root": root, "home": home, "store": filepath.Join(root, "store"), "mount": filepath.Join(root, "mount"), "sessions": copies, "inventory": inventory, "source": "local-session-copies"}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), encoded, 0o600); err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func copyStable(candidate localCandidate, target string) (string, error) {
	source, err := os.Open(candidate.session.RolloutPath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(candidate.info, before) || before.Size() != candidate.info.Size() || !before.ModTime().Equal(candidate.info.ModTime()) {
		return "", errors.New("source changed before copy")
	}
	destination, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(destination, hash), source)
	syncErr := destination.Sync()
	closeErr := destination.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return "", err
	}
	after, err := source.Stat()
	if err != nil {
		return "", err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", errors.New("source changed during copy")
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	verification := sha256.New()
	if _, err := io.Copy(verification, source); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != hex.EncodeToString(verification.Sum(nil)) {
		return "", errors.New("source hash changed during copy")
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		return "", err
	}
	return digest, nil
}

func printJSON(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		fmt.Println(string(encoded))
	}
	return err
}
