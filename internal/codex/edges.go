package codex

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type SpawnEdge struct {
	ParentID string `json:"parent_id"`
	ChildID  string `json:"child_id"`
	Status   string `json:"status"`
}

func LoadSpawnEdges(home string) ([]SpawnEdge, error) {
	dbPath := filepath.Join(home, "state_5.sqlite")
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open Codex spawn-edge database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`pragma busy_timeout = 5000`); err != nil {
		return nil, fmt.Errorf("configure Codex spawn-edge database: %w", err)
	}
	var exists int
	if err := db.QueryRow(`select count(*) from sqlite_master where type = 'table' and name = 'thread_spawn_edges'`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("inspect Codex spawn-edge table: %w", err)
	}
	if exists == 0 {
		return []SpawnEdge{}, nil
	}
	rows, err := db.Query(`
		select parent_thread_id, child_thread_id, status
		from thread_spawn_edges
		order by parent_thread_id, child_thread_id, status
	`)
	if err != nil {
		return nil, fmt.Errorf("query Codex spawn edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	edges := make([]SpawnEdge, 0)
	for rows.Next() {
		var edge SpawnEdge
		if err := rows.Scan(&edge.ParentID, &edge.ChildID, &edge.Status); err != nil {
			return nil, fmt.Errorf("scan Codex spawn edge: %w", err)
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Codex spawn edges: %w", err)
	}
	return edges, nil
}
