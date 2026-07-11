package codex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type RouteTarget struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type RouteOptions struct {
	CodexHome    string
	SessionID    string
	ExpectedPath string
	Target       RouteTarget
}

type RouteResult struct {
	SessionID    string `json:"session_id"`
	PreviousPath string `json:"previous_path"`
	CurrentPath  string `json:"current_path"`
}

func RouteSession(ctx context.Context, options RouteOptions) (RouteResult, error) {
	if options.CodexHome == "" || options.SessionID == "" || options.ExpectedPath == "" || options.Target.Path == "" || options.Target.Bytes < 0 || len(options.Target.SHA256) != 64 {
		return RouteResult{}, errors.New("complete route options and verified target metadata are required")
	}
	if err := verifyRouteTarget(options.Target); err != nil {
		return RouteResult{}, err
	}
	db, err := sql.Open("sqlite", filepath.Join(options.CodexHome, "state_5.sqlite"))
	if err != nil {
		return RouteResult{}, fmt.Errorf("open Codex route database: %w", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return RouteResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `pragma busy_timeout = 10000`); err != nil {
		return RouteResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `begin immediate`); err != nil {
		return RouteResult{}, fmt.Errorf("begin immediate Codex route transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `rollback`)
		}
	}()
	var current string
	if err := conn.QueryRowContext(ctx, `select rollout_path from threads where id = ?`, options.SessionID).Scan(&current); err != nil {
		return RouteResult{}, fmt.Errorf("read current Codex route: %w", err)
	}
	if filepath.Clean(current) != filepath.Clean(options.ExpectedPath) {
		return RouteResult{}, fmt.Errorf("Codex route changed: current=%s expected=%s", current, options.ExpectedPath)
	}
	if err := verifyRouteTarget(options.Target); err != nil {
		return RouteResult{}, fmt.Errorf("revalidate route target inside transaction: %w", err)
	}
	result, err := conn.ExecContext(ctx, `update threads set rollout_path = ? where id = ? and rollout_path = ?`, options.Target.Path, options.SessionID, current)
	if err != nil {
		return RouteResult{}, fmt.Errorf("update Codex route: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RouteResult{}, err
	}
	if rows != 1 {
		return RouteResult{}, fmt.Errorf("Codex route update affected %d rows", rows)
	}
	if _, err := conn.ExecContext(ctx, `commit`); err != nil {
		return RouteResult{}, fmt.Errorf("commit Codex route: %w", err)
	}
	committed = true
	var confirmed string
	if err := conn.QueryRowContext(ctx, `select rollout_path from threads where id = ?`, options.SessionID).Scan(&confirmed); err != nil {
		return RouteResult{}, fmt.Errorf("confirm Codex route: %w", err)
	}
	if filepath.Clean(confirmed) != filepath.Clean(options.Target.Path) {
		return RouteResult{}, errors.New("committed Codex route did not persist")
	}
	return RouteResult{SessionID: options.SessionID, PreviousPath: current, CurrentPath: confirmed}, nil
}

func verifyRouteTarget(target RouteTarget) error {
	file, err := os.Open(target.Path)
	if err != nil {
		return fmt.Errorf("open route target: %w", err)
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if bytesRead != target.Bytes || hex.EncodeToString(hasher.Sum(nil)) != target.SHA256 {
		return errors.New("route target does not match current-byte metadata")
	}
	return nil
}
