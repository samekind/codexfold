package scan

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	_ "modernc.org/sqlite"
)

const dedupIndexBatchSize = 4096

type DedupLayerStats struct {
	Layer                string `json:"layer"`
	ObjectCount          int64  `json:"object_count"`
	UniqueObjectCount    int64  `json:"unique_object_count"`
	TotalBytes           int64  `json:"total_bytes"`
	UniqueBytes          int64  `json:"unique_bytes"`
	DuplicateOccurrences int64  `json:"duplicate_occurrences"`
	DuplicateBytes       int64  `json:"duplicate_bytes"`
}

type DedupObjectStats struct {
	DigestHex      string `json:"digest_sha256"`
	Size           int64  `json:"size"`
	Occurrences    int64  `json:"occurrences"`
	DuplicateBytes int64  `json:"duplicate_bytes"`
	SamplePath     string `json:"sample_path,omitempty"`
}

type dedupIndex struct {
	db      *sql.DB
	tx      *sql.Tx
	observe *sql.Stmt
	pending int
	closed  bool
}

func openDedupIndex(path string) (*dedupIndex, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open dedup sqlite index: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`pragma journal_mode = wal`,
		`pragma synchronous = normal`,
		`pragma temp_store = file`,
		`pragma cache_size = -65536`,
		`create table if not exists dedup_objects (
			layer text not null,
			digest blob not null,
			size integer not null,
			occurrences integer not null,
			sample_path text not null default '',
			primary key (layer, digest, size)
		) without rowid`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize dedup sqlite index: %w", err)
		}
	}

	index := &dedupIndex{db: db}
	if err := index.beginBatch(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return index, nil
}

func (d *dedupIndex) beginBatch() error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin dedup index batch: %w", err)
	}
	stmt, err := tx.Prepare(`
		insert into dedup_objects (layer, digest, size, occurrences, sample_path)
		values (?, ?, ?, 1, ?)
		on conflict (layer, digest, size)
		do update set occurrences = occurrences + 1
	`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare dedup observation: %w", err)
	}
	d.tx = tx
	d.observe = stmt
	d.pending = 0
	return nil
}

func (d *dedupIndex) Observe(layer string, digest [sha256.Size]byte, size int64) error {
	return d.ObserveAt(layer, digest, size, "")
}

func (d *dedupIndex) ObserveAt(layer string, digest [sha256.Size]byte, size int64, samplePath string) error {
	if d.closed {
		return fmt.Errorf("observe closed dedup index")
	}
	if d.observe == nil {
		if err := d.beginBatch(); err != nil {
			return err
		}
	}
	if _, err := d.observe.Exec(layer, digest[:], size, samplePath); err != nil {
		return fmt.Errorf("record dedup observation: %w", err)
	}
	d.pending++
	if d.pending >= dedupIndexBatchSize {
		return d.flush()
	}
	return nil
}

func (d *dedupIndex) flush() error {
	if d.observe == nil {
		return nil
	}
	if err := d.observe.Close(); err != nil {
		return fmt.Errorf("close dedup observation batch: %w", err)
	}
	if err := d.tx.Commit(); err != nil {
		return fmt.Errorf("commit dedup observation batch: %w", err)
	}
	d.tx = nil
	d.observe = nil
	d.pending = 0
	return nil
}

func (d *dedupIndex) LayerStats(layer string) (DedupLayerStats, error) {
	if err := d.flush(); err != nil {
		return DedupLayerStats{}, err
	}
	stats := DedupLayerStats{Layer: layer}
	err := d.db.QueryRow(`
		select
			coalesce(sum(occurrences), 0),
			count(*),
			coalesce(sum(size * occurrences), 0),
			coalesce(sum(size), 0),
			coalesce(sum(occurrences - 1), 0),
			coalesce(sum(size * (occurrences - 1)), 0)
		from dedup_objects
		where layer = ?
	`, layer).Scan(
		&stats.ObjectCount,
		&stats.UniqueObjectCount,
		&stats.TotalBytes,
		&stats.UniqueBytes,
		&stats.DuplicateOccurrences,
		&stats.DuplicateBytes,
	)
	if err != nil {
		return DedupLayerStats{}, fmt.Errorf("query dedup layer stats: %w", err)
	}
	return stats, nil
}

func (d *dedupIndex) TopObjects(layer string, limit int) ([]DedupObjectStats, error) {
	if err := d.flush(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []DedupObjectStats{}, nil
	}
	rows, err := d.db.Query(`
		select digest, size, occurrences, size * (occurrences - 1) as duplicate_bytes, sample_path
		from dedup_objects
		where layer = ? and occurrences > 1
		order by duplicate_bytes desc, size desc, digest
		limit ?
	`, layer, limit)
	if err != nil {
		return nil, fmt.Errorf("query top dedup objects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]DedupObjectStats, 0, limit)
	for rows.Next() {
		var digest []byte
		var item DedupObjectStats
		if err := rows.Scan(&digest, &item.Size, &item.Occurrences, &item.DuplicateBytes, &item.SamplePath); err != nil {
			return nil, fmt.Errorf("scan top dedup object: %w", err)
		}
		item.DigestHex = hex.EncodeToString(digest)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate top dedup objects: %w", err)
	}
	return result, nil
}

func (d *dedupIndex) Close() error {
	if d == nil || d.closed {
		return nil
	}
	d.closed = true
	if err := d.flush(); err != nil {
		_ = d.db.Close()
		return err
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("close dedup sqlite index: %w", err)
	}
	return nil
}
