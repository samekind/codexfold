//go:build ignore

// A byte-preserving full session-tree and SQLite snapshot for real-client tests.
// Opens the production database read-only; does not copy credentials or config.
package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codex"
)

type snapshotFile struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	rootArg := flag.String("root", "", "New workspace-owned fixture directory")
	homeArg := flag.String("source-home", "", "Codex home to open read-only")
	backupOnly := flag.Bool("backup-only", false, "Preserve original SQLite routes for an offline recovery backup")
	flag.Parse()
	home, err := codex.ResolveHome(*homeArg)
	if err != nil {
		return err
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(*rootArg)
	if err != nil || *rootArg == "" {
		return errors.New("a new absolute fixture root is required")
	}
	relative, err := filepath.Rel(home, root)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
		return errors.New("fixture must be outside the production Codex home")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		return err
	}
	clone := filepath.Join(root, "home")
	if err := os.Mkdir(clone, 0700); err != nil {
		return err
	}
	var files []snapshotFile
	var skipped int
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		sourceBase := filepath.Join(home, namespace)
		if *backupOnly {
			info, err := os.Lstat(sourceBase)
			if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				return fmt.Errorf("backup requires ordinary session directories: %s", sourceBase)
			}
		}
		if err := os.MkdirAll(filepath.Join(clone, namespace), 0700); err != nil {
			return err
		}
		err := filepath.WalkDir(sourceBase, func(source string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				skipped++
				return nil
			}
			rel, err := filepath.Rel(home, source)
			if err != nil {
				return err
			}
			target := filepath.Join(clone, rel)
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			digest, err := copySnapshot(source, target, info)
			if err != nil {
				skipped++
				_ = os.Remove(target) // exact file created by this copy only
				return nil
			}
			files = append(files, snapshotFile{source, target, info.Size(), digest})
			return nil
		})
		if err != nil {
			return err
		}
	}
	if *backupOnly && skipped != 0 {
		return fmt.Errorf("backup is incomplete: %d changed or non-regular files", skipped)
	}
	// VACUUM INTO makes one consistent snapshot, including committed WAL data,
	// while the source connection remains mode=ro.
	sourceDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(home, "state_5.sqlite"))+"?mode=ro")
	if err != nil {
		return err
	}
	_, err = sourceDB.Exec(`vacuum into ?`, filepath.Join(clone, "state_5.sqlite"))
	closeErr := sourceDB.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return err
	}
	if *backupOnly {
		var bytes int64
		for _, file := range files {
			bytes += file.Bytes
		}
		metadata := map[string]any{"root": root, "home": clone, "source_home": home, "files": files, "copied_bytes": bytes, "copied_files": len(files), "database_routes_preserved": true}
		encoded, err := json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "backup.json"), encoded, 0600); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"root": root, "copied_files": len(files), "copied_bytes": bytes, "database_routes_preserved": true})
	}
	db, err := sql.Open("sqlite", filepath.Join(clone, "state_5.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`select id, rollout_path from threads`)
	if err != nil {
		return err
	}
	type route struct{ id, path string }
	var routes []route
	for rows.Next() {
		var r route
		if err := rows.Scan(&r.id, &r.path); err != nil {
			rows.Close()
			return err
		}
		routes = append(routes, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range routes {
		rel, err := filepath.Rel(home, r.path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			// Never let a test client route writes to an external original file.
			if _, err := db.Exec(`delete from threads where id=?`, r.id); err != nil {
				return err
			}
			continue
		}
		if _, err := db.Exec(`update threads set rollout_path=? where id=?`, filepath.Join(clone, rel), r.id); err != nil {
			return err
		}
	}
	if err := db.Close(); err != nil {
		return err
	}
	sessions, err := codex.LoadSessions(clone)
	if err != nil {
		return err
	}
	var candidates []codex.Session
	for _, s := range sessions {
		info, err := os.Stat(s.RolloutPath)
		if err != nil || info.Size() == 0 || s.UpdatedAt > time.Now().Add(-time.Hour).Unix() {
			continue
		}
		file, err := os.Open(s.RolloutPath)
		if err != nil {
			continue
		}
		first, readErr := bufio.NewReader(file).ReadBytes('\n')
		file.Close()
		var header struct {
			Type    string `json:"type"`
			Payload struct {
				ID     string          `json:"id"`
				Source json.RawMessage `json:"source"`
			} `json:"payload"`
		}
		if readErr != nil || json.Unmarshal(first, &header) != nil || header.Type != "session_meta" || header.Payload.ID != s.ID {
			continue
		}
		var source string
		if json.Unmarshal(header.Payload.Source, &source) != nil {
			continue
		} // multi-agent child histories need their parent
		candidates = append(candidates, s)
	}
	if len(candidates) == 0 {
		return errors.New("no stable main thread is available for isolated real-client testing")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].UpdatedAt > candidates[j].UpdatedAt })
	var bytes int64
	for _, f := range files {
		bytes += f.Bytes
	}
	metadata := map[string]any{"root": root, "home": clone, "store": filepath.Join(root, "store"), "native_root": filepath.Join(root, "native"), "mount": "T:\\", "files": files, "source_home": home, "copied_bytes": bytes, "copied_files": len(files), "skipped_files": skipped, "session": candidates[0], "source": "full-local-session-copy"}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "full-fixture.json"), encoded, 0600); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"root": root, "copied_files": len(files), "copied_bytes": bytes, "skipped_files": skipped, "threads": len(sessions), "main_thread": candidates[0].ID})
}

func copySnapshot(sourcePath, target string, expected os.FileInfo) (string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, expected) || before.Size() != expected.Size() || !before.ModTime().Equal(expected.ModTime()) {
		return "", errors.New("source changed")
	}
	destination, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(destination, hash), source)
	err = errors.Join(copyErr, destination.Sync(), destination.Close())
	if err != nil {
		return "", err
	}
	after, err := source.Stat()
	if err != nil {
		return "", err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", errors.New("source changed")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash.Reset()
	if _, err := io.Copy(hash, source); err != nil {
		return "", err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return "", errors.New("source digest changed")
	}
	return digest, os.Chtimes(target, before.ModTime(), before.ModTime())
}
