package sessionns

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/samekind/codexfold/internal/mountfs"

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
	var result strings.Builder
	result.WriteString("case ")
	for _, prefix := range routePrefixes(options) {
		from := quoteSQLString(prefix.mount)
		fmt.Fprintf(&result, "when %s then %s || substr(%s, length(%s) + 1) ", routePrefixCondition(value, from), quoteSQLString(prefix.home), value, from)
	}
	fmt.Fprintf(&result, "else %s end", value)
	return result.String()
}

func routeGuardCondition(options Options, value string) string {
	var conditions []string
	for _, prefix := range routePrefixes(options) {
		conditions = append(conditions, routePrefixCondition(value, quoteSQLString(prefix.mount)))
	}
	return strings.Join(conditions, " or ")
}

type routePrefix struct{ mount, home string }

func routePrefixes(options Options) []routePrefix {
	roots := []string{options.Mount}
	if runtime.GOOS == "windows" && len(filepath.Clean(options.Mount)) == 3 && filepath.Dir(options.Mount) == options.Mount {
		unc := mountfs.WindowsUNCPath(options.Home, options.Mount)
		roots = append(roots, `\\?\`+filepath.Clean(options.Mount), unc, `\\?\UNC\`+strings.TrimPrefix(unc, `\\`))
	}
	var prefixes []routePrefix
	for _, root := range roots {
		for _, namespace := range sessionDirectories {
			prefixes = append(prefixes, routePrefix{filepath.Join(root, namespace) + string(filepath.Separator), filepath.Join(options.Home, namespace) + string(filepath.Separator)})
		}
	}
	return prefixes
}

func routePrefixCondition(value, prefix string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("lower(substr(%s, 1, length(%s))) = lower(%s)", value, prefix, prefix)
	}
	return fmt.Sprintf("substr(%s, 1, length(%s)) = %s", value, prefix, prefix)
}

func quoteSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
