package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	_ "modernc.org/sqlite"
)

type Options struct {
	Apply        bool
	Now          time.Time
	WriterActive func(context.Context, codex.Session) (bool, error)
	BeforeRename func() error
	AfterRename  func() error
}

type Result struct {
	SessionID  string `json:"session_id"`
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	DryRun     bool   `json:"dry_run"`
	Archived   bool   `json:"archived"`
}

type RecoveryResult struct {
	SessionID  string `json:"session_id"`
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
	RolledBack bool   `json:"rolled_back"`
	Finalized  bool   `json:"finalized"`
}

type phase string

const (
	phasePrepared phase = "prepared"
	phaseRenamed  phase = "renamed"
)

type journal struct {
	Version    int    `json:"version"`
	SessionID  string `json:"session_id"`
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Phase      phase  `json:"phase"`
}

type snapshot struct {
	Bytes  int64
	SHA256 string
}

type threadRow struct {
	RolloutPath string
	Archived    bool
}

var commitArchiveTransaction = func(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, `commit`)
	return err
}

func JournalPath(store string, sessionID string) string {
	return filepath.Join(filepath.Clean(store), "archive", "journals", sessionID+".json")
}

func Archive(ctx context.Context, home string, store string, session codex.Session, options Options) (Result, error) {
	if !validSessionID(session.ID) || !filepath.IsAbs(home) || !filepath.IsAbs(store) || !filepath.IsAbs(session.RolloutPath) {
		return Result{}, errors.New("absolute home, store, rollout path, and safe session ID are required")
	}
	home = filepath.Clean(home)
	store = filepath.Clean(store)
	sourcePath := filepath.Clean(session.RolloutPath)
	if session.Archived {
		return Result{}, errors.New("session is already archived")
	}
	if _, err := relativeWithin(filepath.Join(home, "sessions"), sourcePath); err != nil {
		return Result{}, errors.New("active rollout is outside the Codex sessions directory")
	}
	targetPath := filepath.Join(home, "archived_sessions", filepath.Base(sourcePath))
	if sourcePath == targetPath {
		return Result{}, errors.New("archive source and target must differ")
	}
	current, err := hashPath(sourcePath)
	if err != nil {
		return Result{}, fmt.Errorf("hash active rollout: %w", err)
	}
	result := Result{
		SessionID: session.ID, SourcePath: sourcePath, TargetPath: targetPath,
		Bytes: current.Bytes, SHA256: current.SHA256, DryRun: !options.Apply,
	}
	if _, err := os.Lstat(targetPath); err == nil {
		return Result{}, errors.New("archive target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	db, err := openStateDB(home)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = db.Close() }()
	row, err := readThread(ctx, db, session.ID)
	if err != nil {
		return Result{}, err
	}
	if row.Archived || filepath.Clean(row.RolloutPath) != sourcePath {
		return Result{}, errors.New("selected Codex thread is no longer active at the expected rollout")
	}
	if options.Apply && options.WriterActive == nil {
		return Result{}, errors.New("archive apply requires a native writer probe")
	}
	if active, err := writerActive(ctx, session, options.WriterActive); err != nil {
		return Result{}, err
	} else if active {
		return Result{}, errors.New("cannot archive a session with an active writer")
	}
	if !options.Apply {
		return result, nil
	}
	journalPath := JournalPath(store, session.ID)
	if _, err := os.Lstat(journalPath); err == nil {
		return Result{}, errors.New("pending archive journal already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return Result{}, err
	}
	pending := journal{
		Version: 1, SessionID: session.ID, SourcePath: sourcePath, TargetPath: targetPath,
		Bytes: current.Bytes, SHA256: current.SHA256, Phase: phasePrepared,
	}
	if err := writeJournal(journalPath, pending); err != nil {
		return Result{}, err
	}
	journalOwned := true
	removeJournal := func() error {
		if !journalOwned {
			return nil
		}
		if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		journalOwned = false
		return syncArchiveDirectory(filepath.Dir(journalPath))
	}
	if options.BeforeRename != nil {
		if err := options.BeforeRename(); err != nil {
			return Result{}, errors.Join(err, removeJournal())
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return Result{}, errors.Join(err, removeJournal())
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `begin immediate`); err != nil {
		return Result{}, errors.Join(fmt.Errorf("begin Codex archive transaction: %w", err), removeJournal())
	}
	transactionClosed := false
	defer func() {
		if !transactionClosed {
			_, _ = conn.ExecContext(context.Background(), `rollback`)
		}
	}()
	row, err = readThreadConn(ctx, conn, session.ID)
	if err != nil {
		return Result{}, errors.Join(err, removeJournal())
	}
	if row.Archived || filepath.Clean(row.RolloutPath) != sourcePath {
		return Result{}, errors.Join(errors.New("Codex route or archive state changed before rename"), removeJournal())
	}
	verified, err := hashPath(sourcePath)
	if err != nil || verified != current {
		if err == nil {
			err = errors.New("active rollout changed before archive rename")
		}
		return Result{}, errors.Join(err, removeJournal())
	}
	if active, err := writerActive(ctx, session, options.WriterActive); err != nil {
		return Result{}, errors.Join(err, removeJournal())
	} else if active {
		return Result{}, errors.Join(errors.New("cannot archive a session with an active writer"), removeJournal())
	}
	if _, err := os.Lstat(targetPath); err == nil {
		return Result{}, errors.Join(errors.New("archive target appeared before rename"), removeJournal())
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, errors.Join(err, removeJournal())
	}
	if err := os.Rename(sourcePath, targetPath); err != nil {
		return Result{}, errors.Join(fmt.Errorf("rename rollout into archive: %w", err), removeJournal())
	}
	renamed := true
	rollbackFile := func() error {
		if !renamed {
			return nil
		}
		if _, err := os.Lstat(sourcePath); err == nil {
			return errors.New("cannot roll back archive while source path exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		matches, err := pathMatches(targetPath, current)
		if err != nil {
			return err
		}
		if !matches {
			return errors.New("cannot roll back archive because target bytes changed")
		}
		if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
			return err
		}
		if err := os.Rename(targetPath, sourcePath); err != nil {
			return err
		}
		renamed = false
		return syncArchiveRename(targetPath, sourcePath)
	}
	if err := syncArchiveRename(sourcePath, targetPath); err != nil {
		return Result{}, errors.Join(err, rollbackFile(), removeJournal())
	}
	pending.Phase = phaseRenamed
	if err := writeJournal(journalPath, pending); err != nil {
		return Result{}, errors.Join(err, rollbackFile(), removeJournal())
	}
	if options.AfterRename != nil {
		if err := options.AfterRename(); err != nil {
			return Result{}, errors.Join(err, rollbackFile(), removeJournal())
		}
	}
	columns, err := threadColumns(ctx, conn)
	if err != nil {
		return Result{}, errors.Join(err, rollbackFile(), removeJournal())
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	query := `update threads set rollout_path = ?, archived = 1`
	args := []any{targetPath}
	if columns["archived_at"] {
		query += `, archived_at = ?`
		args = append(args, now.Unix())
	}
	if columns["updated_at_ms"] {
		var maximum sql.NullInt64
		if err := conn.QueryRowContext(ctx, `select max(updated_at_ms) from threads`).Scan(&maximum); err != nil {
			return Result{}, errors.Join(err, rollbackFile(), removeJournal())
		}
		if maximum.Valid && maximum.Int64 == math.MaxInt64 {
			return Result{}, errors.Join(errors.New("thread update clock overflow"), rollbackFile(), removeJournal())
		}
		next := int64(1)
		if maximum.Valid {
			next = maximum.Int64 + 1
		}
		query += `, updated_at_ms = ?`
		args = append(args, next)
	}
	query += ` where id = ? and rollout_path = ? and archived = 0`
	args = append(args, session.ID, sourcePath)
	update, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return Result{}, errors.Join(err, rollbackFile(), removeJournal())
	}
	rows, err := update.RowsAffected()
	if err != nil || rows != 1 {
		if err == nil {
			err = fmt.Errorf("archive update affected %d rows", rows)
		}
		return Result{}, errors.Join(err, rollbackFile(), removeJournal())
	}
	if err := commitArchiveTransaction(ctx, conn); err != nil {
		_, _ = conn.ExecContext(context.Background(), `rollback`)
		transactionClosed = true
		verifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		after, stateErr := readThread(verifyCtx, db, session.ID)
		if stateErr != nil {
			return result, errors.Join(fmt.Errorf("archive commit outcome is unknown: %w", err), stateErr)
		}
		switch {
		case after.Archived && filepath.Clean(after.RolloutPath) == targetPath:
			renamed = false
			result.DryRun = false
			result.Archived = true
			return result, fmt.Errorf("archive committed but commit acknowledgement failed; run archive recover --apply: %w", err)
		case !after.Archived && filepath.Clean(after.RolloutPath) == sourcePath:
			fileErr := rollbackFile()
			var journalErr error
			if fileErr == nil {
				journalErr = removeJournal()
			}
			return result, errors.Join(fmt.Errorf("commit Codex archive transaction: %w", err), fileErr, journalErr)
		default:
			return result, errors.Join(fmt.Errorf("archive commit outcome is ambiguous: %w", err), errors.New("Codex thread route no longer matches either archive state"))
		}
	}
	transactionClosed = true
	renamed = false
	result.DryRun = false
	result.Archived = true
	if err := removeJournal(); err != nil {
		return result, err
	}
	return result, nil
}

func Recover(ctx context.Context, home string, store string, sessionID string) (RecoveryResult, error) {
	if !validSessionID(sessionID) || !filepath.IsAbs(home) || !filepath.IsAbs(store) {
		return RecoveryResult{}, errors.New("absolute home and store paths and a safe session ID are required")
	}
	home = filepath.Clean(home)
	store = filepath.Clean(store)
	path := JournalPath(store, sessionID)
	pending, err := readJournal(path)
	if err != nil {
		return RecoveryResult{}, err
	}
	if pending.SessionID != sessionID || !validSessionID(sessionID) {
		return RecoveryResult{}, errors.New("archive journal session does not match recovery request")
	}
	if _, err := relativeWithin(filepath.Join(home, "sessions"), pending.SourcePath); err != nil {
		return RecoveryResult{}, errors.New("archive journal source is outside the Codex sessions directory")
	}
	expectedTarget := filepath.Join(home, "archived_sessions", filepath.Base(pending.SourcePath))
	if filepath.Clean(pending.TargetPath) != expectedTarget {
		return RecoveryResult{}, errors.New("archive journal target does not match the official flat archive path")
	}
	result := RecoveryResult{SessionID: sessionID, SourcePath: pending.SourcePath, TargetPath: pending.TargetPath}
	db, err := openStateDB(home)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `begin immediate`); err != nil {
		return RecoveryResult{}, fmt.Errorf("begin Codex archive recovery transaction: %w", err)
	}
	transactionClosed := false
	defer func() {
		if !transactionClosed {
			_, _ = conn.ExecContext(context.Background(), `rollback`)
		}
	}()
	row, err := readThreadConn(ctx, conn, sessionID)
	if err != nil {
		return RecoveryResult{}, err
	}
	want := snapshot{Bytes: pending.Bytes, SHA256: pending.SHA256}
	sourceExists, sourceMatches, err := inspectPath(pending.SourcePath, want)
	if err != nil {
		return RecoveryResult{}, err
	}
	targetExists, targetMatches, err := inspectPath(pending.TargetPath, want)
	if err != nil {
		return RecoveryResult{}, err
	}
	switch {
	case !row.Archived && filepath.Clean(row.RolloutPath) == filepath.Clean(pending.SourcePath):
		switch {
		case sourceExists && sourceMatches && !targetExists:
		case !sourceExists && targetExists && targetMatches:
			if err := os.MkdirAll(filepath.Dir(pending.SourcePath), 0o700); err != nil {
				return RecoveryResult{}, err
			}
			if err := os.Rename(pending.TargetPath, pending.SourcePath); err != nil {
				return RecoveryResult{}, err
			}
			if err := syncArchiveRename(pending.TargetPath, pending.SourcePath); err != nil {
				return RecoveryResult{}, err
			}
		default:
			return RecoveryResult{}, errors.New("archive rollback state is ambiguous or changed")
		}
		result.RolledBack = true
	case row.Archived && filepath.Clean(row.RolloutPath) == filepath.Clean(pending.TargetPath):
		if sourceExists || !targetExists || !targetMatches {
			return RecoveryResult{}, errors.New("committed archive files are ambiguous or changed")
		}
		result.Finalized = true
	default:
		return RecoveryResult{}, errors.New("Codex thread state no longer matches the archive journal")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return RecoveryResult{}, err
	}
	if err := syncArchiveDirectory(filepath.Dir(path)); err != nil {
		return RecoveryResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `commit`); err != nil {
		return RecoveryResult{}, fmt.Errorf("commit Codex archive recovery transaction: %w", err)
	}
	transactionClosed = true
	return result, nil
}

func openStateDB(home string) (*sql.DB, error) {
	dbPath := filepath.Join(filepath.Clean(home), "state_5.sqlite")
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, fmt.Errorf("locate Codex archive database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("Codex archive database is not a regular file")
	}
	db, err := sql.Open("sqlite", sqliteReadWriteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open Codex archive database: %w", err)
	}
	if _, err := db.Exec(`pragma busy_timeout = 10000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func sqliteReadWriteDSN(path string) string {
	slashPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		slashPath = strings.ReplaceAll(path, "\\", "/")
		if !strings.HasPrefix(slashPath, "/") {
			slashPath = "/" + slashPath
		}
	}
	uri := &url.URL{Scheme: "file", Path: slashPath}
	query := uri.Query()
	query.Set("mode", "rw")
	uri.RawQuery = query.Encode()
	return uri.String()
}

func readThread(ctx context.Context, db *sql.DB, sessionID string) (threadRow, error) {
	var row threadRow
	var archived int
	if err := db.QueryRowContext(ctx, `select rollout_path, archived from threads where id = ?`, sessionID).Scan(&row.RolloutPath, &archived); err != nil {
		return threadRow{}, fmt.Errorf("read Codex archive thread: %w", err)
	}
	row.Archived = archived != 0
	return row, nil
}

func readThreadConn(ctx context.Context, conn *sql.Conn, sessionID string) (threadRow, error) {
	var row threadRow
	var archived int
	if err := conn.QueryRowContext(ctx, `select rollout_path, archived from threads where id = ?`, sessionID).Scan(&row.RolloutPath, &archived); err != nil {
		return threadRow{}, fmt.Errorf("revalidate Codex archive thread: %w", err)
	}
	row.Archived = archived != 0
	return row, nil
}

func threadColumns(ctx context.Context, conn *sql.Conn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `pragma table_info(threads)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func writerActive(ctx context.Context, session codex.Session, probe func(context.Context, codex.Session) (bool, error)) (bool, error) {
	if probe == nil {
		return false, nil
	}
	active, err := probe(ctx, session)
	if err != nil {
		return false, fmt.Errorf("probe archive writer: %w", err)
	}
	return active, nil
}

func hashPath(path string) (snapshot, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return snapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return snapshot{}, errors.New("rollout path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return snapshot{}, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return snapshot{}, errors.Join(copyErr, closeErr)
	}
	return snapshot{Bytes: written, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func pathMatches(path string, want snapshot) (bool, error) {
	got, err := hashPath(path)
	if err != nil {
		return false, err
	}
	return got == want, nil
}

func inspectPath(path string, want snapshot) (bool, bool, error) {
	got, err := hashPath(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, got == want, nil
}

func validSessionID(sessionID string) bool {
	return sessionID != "" && sessionID != "." && sessionID != ".." && filepath.Base(sessionID) == sessionID && !strings.ContainsAny(sessionID, "/\\\x00")
}

func relativeWithin(root string, target string) (string, error) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("path is outside root")
	}
	return relative, nil
}

func syncArchiveRename(sourcePath string, targetPath string) error {
	sourceDirectory := filepath.Dir(sourcePath)
	targetDirectory := filepath.Dir(targetPath)
	if err := syncArchiveDirectory(sourceDirectory); err != nil {
		return err
	}
	if targetDirectory == sourceDirectory {
		return nil
	}
	return syncArchiveDirectory(targetDirectory)
}

func writeJournal(path string, value journal) error {
	if value.Version != 1 || !validSessionID(value.SessionID) || !filepath.IsAbs(value.SourcePath) || !filepath.IsAbs(value.TargetPath) || value.Bytes < 0 || len(value.SHA256) != 64 || value.Phase != phasePrepared && value.Phase != phaseRenamed {
		return errors.New("complete archive journal metadata is required")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".archive-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncArchiveDirectory(directory)
}

func readJournal(path string) (journal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return journal{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value journal
	if err := decoder.Decode(&value); err != nil {
		return journal{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return journal{}, err
	}
	if value.Version != 1 || !validSessionID(value.SessionID) || !filepath.IsAbs(value.SourcePath) || !filepath.IsAbs(value.TargetPath) || value.Bytes < 0 || len(value.SHA256) != 64 || value.Phase != phasePrepared && value.Phase != phaseRenamed {
		return journal{}, errors.New("invalid archive journal")
	}
	return value, nil
}
