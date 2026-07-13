package sessionns

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	routeInsertTrigger = "codexfold_normalize_rollout_path_insert"
	routeUpdateTrigger = "codexfold_normalize_rollout_path_update"
)

func installRouteGuard(options Options) error {
	return updateRouteGuard(options, true)
}

func removeRouteGuard(options Options) error {
	return updateRouteGuard(options, false)
}

func updateRouteGuard(options Options, install bool) error {
	databasePath := filepath.Join(options.Home, "state_5.sqlite")
	if _, err := os.Stat(databasePath); err != nil {
		if !install && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("locate Codex state database: %w", err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return fmt.Errorf("open Codex state database: %w", err)
	}
	defer database.Close()
	connection, err := database.Conn(context.Background())
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.ExecContext(context.Background(), `pragma busy_timeout = 10000`); err != nil {
		return err
	}
	if _, err := connection.ExecContext(context.Background(), `begin immediate`); err != nil {
		return fmt.Errorf("begin route guard transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = connection.ExecContext(context.Background(), `rollback`)
		}
	}()
	for _, name := range []string{routeInsertTrigger, routeUpdateTrigger} {
		if _, err := connection.ExecContext(context.Background(), `drop trigger if exists `+name); err != nil {
			return err
		}
	}
	if install {
		for _, statement := range routeGuardTriggerStatements(options) {
			if _, err := connection.ExecContext(context.Background(), statement); err != nil {
				return fmt.Errorf("install Codex route guard: %w", err)
			}
		}
	}
	if _, err := connection.ExecContext(context.Background(), normalizeExistingRoutesStatement(options)); err != nil {
		return fmt.Errorf("normalize existing Codex routes: %w", err)
	}
	if _, err := connection.ExecContext(context.Background(), `commit`); err != nil {
		return fmt.Errorf("commit route guard transaction: %w", err)
	}
	committed = true
	return nil
}

func routeGuardTriggerStatements(options Options) []string {
	body := routeGuardBody(options)
	condition := routeGuardCondition(options, "NEW.rollout_path")
	return []string{
		fmt.Sprintf(`create trigger %s after insert on threads when %s begin %s end`, routeInsertTrigger, condition, body),
		fmt.Sprintf(`create trigger %s after update of rollout_path on threads when %s begin %s end`, routeUpdateTrigger, condition, body),
	}
}

func routeGuardBody(options Options) string {
	return fmt.Sprintf(`update threads set rollout_path = %s where id = NEW.id;`, routeGuardCase(options, "NEW.rollout_path"))
}

func normalizeExistingRoutesStatement(options Options) string {
	return fmt.Sprintf(`update threads set rollout_path = %s where %s`, routeGuardCase(options, "rollout_path"), routeGuardCondition(options, "rollout_path"))
}

func routeGuardCase(options Options, value string) string {
	activeMount := filepath.Join(options.Mount, "sessions") + string(filepath.Separator)
	archiveMount := filepath.Join(options.Mount, "archived_sessions") + string(filepath.Separator)
	activeHome := filepath.Join(options.Home, "sessions") + string(filepath.Separator)
	archiveHome := filepath.Join(options.Home, "archived_sessions") + string(filepath.Separator)
	activeMountSQL := quoteSQLString(activeMount)
	archiveMountSQL := quoteSQLString(archiveMount)
	return fmt.Sprintf(
		`case when substr(%s, 1, length(%s)) = %s then %s || substr(%s, length(%s) + 1) else %s || substr(%s, length(%s) + 1) end`,
		value, activeMountSQL, activeMountSQL, quoteSQLString(activeHome), value, activeMountSQL,
		quoteSQLString(archiveHome), value, archiveMountSQL,
	)
}

func routeGuardCondition(options Options, value string) string {
	activeMount := filepath.Join(options.Mount, "sessions") + string(filepath.Separator)
	archiveMount := filepath.Join(options.Mount, "archived_sessions") + string(filepath.Separator)
	activeMountSQL := quoteSQLString(activeMount)
	archiveMountSQL := quoteSQLString(archiveMount)
	return fmt.Sprintf(
		`substr(%s, 1, length(%s)) = %s or substr(%s, 1, length(%s)) = %s`,
		value, activeMountSQL, activeMountSQL,
		value, archiveMountSQL, archiveMountSQL,
	)
}

func quoteSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
