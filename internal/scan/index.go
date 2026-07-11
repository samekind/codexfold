package scan

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

const dedupIndexSchemaVersion = 2

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

type scannedFileState struct {
	Path            string
	Size            int64
	ModTimeNanos    int64
	PrefixSHA256    string
	EndsWithNewline bool
	Stats           DedupFileStats
}

type dedupIndex struct {
	db          *sql.DB
	tx          *sql.Tx
	observe     *sql.Stmt
	currentFile string
	closed      bool
}

func openDedupIndex(path string) (*dedupIndex, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open dedup sqlite index: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := initializeDedupIndex(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &dedupIndex{db: db, currentFile: "__adhoc__"}, nil
}

func initializeDedupIndex(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`pragma user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read dedup index schema version: %w", err)
	}
	if version != 0 && version != dedupIndexSchemaVersion {
		return fmt.Errorf("%w: unsupported scan index schema %d", ErrIndexRebuildRequired, version)
	}
	if version == 0 {
		var existing int
		if err := db.QueryRow(`select count(*) from sqlite_master where type = 'table' and name = 'dedup_objects'`).Scan(&existing); err != nil {
			return fmt.Errorf("inspect scan index schema: %w", err)
		}
		if existing != 0 {
			return fmt.Errorf("%w: legacy scan index schema", ErrIndexRebuildRequired)
		}
	}
	for _, statement := range []string{
		`pragma journal_mode = wal`,
		`pragma synchronous = normal`,
		`pragma temp_store = file`,
		`pragma cache_size = -65536`,
		`create table if not exists dedup_objects (
			file_path text not null,
			layer text not null,
			digest blob not null,
			size integer not null,
			occurrences integer not null,
			sample_path text not null default '',
			primary key (file_path, layer, digest, size)
		) without rowid`,
		`create table if not exists scan_metadata (
			key text primary key,
			value text not null
		) without rowid`,
		`create table if not exists scanned_files (
			path text primary key,
			size integer not null,
			mtime_ns integer not null,
			prefix_sha256 text not null,
			ends_with_newline integer not null,
			scanned_bytes integer not null,
			record_count integer not null,
			parsed_record_count integer not null,
			oversized_record_count integer not null,
			invalid_json_record_count integer not null,
			field_count integer not null,
			field_bytes integer not null,
			cdc_chunk_count integer not null,
			cdc_bytes integer not null
		) without rowid`,
		fmt.Sprintf(`pragma user_version = %d`, dedupIndexSchemaVersion),
	} {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("initialize dedup sqlite index: %w", err)
		}
	}
	return nil
}

func (d *dedupIndex) BeginFile(path string) error {
	if d.closed {
		return errors.New("begin file on closed dedup index")
	}
	if d.tx != nil {
		return errors.New("dedup index already has an active file transaction")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin dedup file transaction: %w", err)
	}
	stmt, err := tx.Prepare(`
		insert into dedup_objects (file_path, layer, digest, size, occurrences, sample_path)
		values (?, ?, ?, ?, 1, ?)
		on conflict (file_path, layer, digest, size)
		do update set occurrences = occurrences + 1
	`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare dedup observation: %w", err)
	}
	d.tx = tx
	d.observe = stmt
	d.currentFile = path
	return nil
}

func (d *dedupIndex) ensureTransaction() error {
	if d.tx != nil {
		return nil
	}
	return d.BeginFile(d.currentFile)
}

func (d *dedupIndex) Observe(layer string, digest [sha256.Size]byte, size int64) error {
	return d.ObserveAt(layer, digest, size, "")
}

func (d *dedupIndex) ObserveAt(layer string, digest [sha256.Size]byte, size int64, samplePath string) error {
	if d.closed {
		return errors.New("observe closed dedup index")
	}
	if err := d.ensureTransaction(); err != nil {
		return err
	}
	if _, err := d.observe.Exec(d.currentFile, layer, digest[:], size, samplePath); err != nil {
		return fmt.Errorf("record dedup observation: %w", err)
	}
	return nil
}

func (d *dedupIndex) DeleteFileLayers(layers map[string]bool) error {
	if err := d.ensureTransaction(); err != nil {
		return err
	}
	for layer := range layers {
		if _, err := d.tx.Exec(`delete from dedup_objects where file_path = ? and layer = ?`, d.currentFile, layer); err != nil {
			return fmt.Errorf("reset %s observations for %s: %w", layer, d.currentFile, err)
		}
	}
	return nil
}

func (d *dedupIndex) SaveFileState(state scannedFileState) error {
	if err := d.ensureTransaction(); err != nil {
		return err
	}
	_, err := d.tx.Exec(`
		insert into scanned_files (
			path, size, mtime_ns, prefix_sha256, ends_with_newline,
			scanned_bytes, record_count, parsed_record_count, oversized_record_count,
			invalid_json_record_count, field_count, field_bytes, cdc_chunk_count, cdc_bytes
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict (path) do update set
			size = excluded.size,
			mtime_ns = excluded.mtime_ns,
			prefix_sha256 = excluded.prefix_sha256,
			ends_with_newline = excluded.ends_with_newline,
			scanned_bytes = excluded.scanned_bytes,
			record_count = excluded.record_count,
			parsed_record_count = excluded.parsed_record_count,
			oversized_record_count = excluded.oversized_record_count,
			invalid_json_record_count = excluded.invalid_json_record_count,
			field_count = excluded.field_count,
			field_bytes = excluded.field_bytes,
			cdc_chunk_count = excluded.cdc_chunk_count,
			cdc_bytes = excluded.cdc_bytes
	`, state.Path, state.Size, state.ModTimeNanos, state.PrefixSHA256, state.EndsWithNewline,
		state.Stats.ScannedBytes, state.Stats.RecordCount, state.Stats.ParsedRecordCount,
		state.Stats.OversizedRecordCount, state.Stats.InvalidJSONRecordCount,
		state.Stats.FieldCount, state.Stats.FieldBytes, state.Stats.CDCChunkCount, state.Stats.CDCBytes)
	if err != nil {
		return fmt.Errorf("save scanned file state: %w", err)
	}
	return nil
}

func (d *dedupIndex) CommitFile() error {
	if d.tx == nil {
		return nil
	}
	if err := d.observe.Close(); err != nil {
		_ = d.tx.Rollback()
		d.clearTransaction()
		return fmt.Errorf("close dedup observation statement: %w", err)
	}
	if err := d.tx.Commit(); err != nil {
		d.clearTransaction()
		return fmt.Errorf("commit dedup file transaction: %w", err)
	}
	d.clearTransaction()
	return nil
}

func (d *dedupIndex) RollbackFile() error {
	if d.tx == nil {
		return nil
	}
	_ = d.observe.Close()
	err := d.tx.Rollback()
	d.clearTransaction()
	if err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("rollback dedup file transaction: %w", err)
	}
	return nil
}

func (d *dedupIndex) clearTransaction() {
	d.tx = nil
	d.observe = nil
}

func (d *dedupIndex) LoadFileState(path string) (scannedFileState, bool, error) {
	if d.tx != nil {
		return scannedFileState{}, false, errors.New("cannot load file state during active transaction")
	}
	var state scannedFileState
	state.Path = path
	err := d.db.QueryRow(`
		select size, mtime_ns, prefix_sha256, ends_with_newline,
			scanned_bytes, record_count, parsed_record_count, oversized_record_count,
			invalid_json_record_count, field_count, field_bytes, cdc_chunk_count, cdc_bytes
		from scanned_files where path = ?
	`, path).Scan(&state.Size, &state.ModTimeNanos, &state.PrefixSHA256, &state.EndsWithNewline,
		&state.Stats.ScannedBytes, &state.Stats.RecordCount, &state.Stats.ParsedRecordCount,
		&state.Stats.OversizedRecordCount, &state.Stats.InvalidJSONRecordCount,
		&state.Stats.FieldCount, &state.Stats.FieldBytes, &state.Stats.CDCChunkCount, &state.Stats.CDCBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return scannedFileState{}, false, nil
	}
	if err != nil {
		return scannedFileState{}, false, fmt.Errorf("load scanned file state: %w", err)
	}
	return state, true, nil
}

func (d *dedupIndex) EnsureConfiguration(configuration string) error {
	if d.tx != nil {
		return errors.New("cannot configure index during active transaction")
	}
	var current string
	err := d.db.QueryRow(`select value from scan_metadata where key = 'configuration'`).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := d.db.Exec(`insert into scan_metadata (key, value) values ('configuration', ?)`, configuration); err != nil {
			return fmt.Errorf("save scan index configuration: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load scan index configuration: %w", err)
	}
	if current != configuration {
		return fmt.Errorf("%w: scan settings changed", ErrIndexRebuildRequired)
	}
	return nil
}

func (d *dedupIndex) CorpusStats() (DedupFileStats, error) {
	if err := d.CommitFile(); err != nil {
		return DedupFileStats{}, err
	}
	var stats DedupFileStats
	err := d.db.QueryRow(`
		select coalesce(sum(scanned_bytes), 0), coalesce(sum(record_count), 0),
			coalesce(sum(parsed_record_count), 0), coalesce(sum(oversized_record_count), 0),
			coalesce(sum(invalid_json_record_count), 0), coalesce(sum(field_count), 0),
			coalesce(sum(field_bytes), 0), coalesce(sum(cdc_chunk_count), 0),
			coalesce(sum(cdc_bytes), 0)
		from scanned_files
	`).Scan(&stats.ScannedBytes, &stats.RecordCount, &stats.ParsedRecordCount,
		&stats.OversizedRecordCount, &stats.InvalidJSONRecordCount, &stats.FieldCount,
		&stats.FieldBytes, &stats.CDCChunkCount, &stats.CDCBytes)
	if err != nil {
		return DedupFileStats{}, fmt.Errorf("query scan corpus stats: %w", err)
	}
	return stats, nil
}

func (d *dedupIndex) LayerStats(layer string) (DedupLayerStats, error) {
	if err := d.CommitFile(); err != nil {
		return DedupLayerStats{}, err
	}
	stats := DedupLayerStats{Layer: layer}
	err := d.db.QueryRow(`
		with objects as (
			select digest, size, sum(occurrences) as occurrences
			from dedup_objects where layer = ? group by digest, size
		)
		select coalesce(sum(occurrences), 0), count(*),
			coalesce(sum(size * occurrences), 0), coalesce(sum(size), 0),
			coalesce(sum(occurrences - 1), 0), coalesce(sum(size * (occurrences - 1)), 0)
		from objects
	`, layer).Scan(&stats.ObjectCount, &stats.UniqueObjectCount, &stats.TotalBytes,
		&stats.UniqueBytes, &stats.DuplicateOccurrences, &stats.DuplicateBytes)
	if err != nil {
		return DedupLayerStats{}, fmt.Errorf("query dedup layer stats: %w", err)
	}
	return stats, nil
}

func (d *dedupIndex) TopObjects(layer string, limit int) ([]DedupObjectStats, error) {
	if err := d.CommitFile(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []DedupObjectStats{}, nil
	}
	rows, err := d.db.Query(`
		with objects as (
			select digest, size, sum(occurrences) as occurrences,
				min(case when sample_path != '' then sample_path end) as sample_path
			from dedup_objects where layer = ? group by digest, size
		)
		select digest, size, occurrences, size * (occurrences - 1), coalesce(sample_path, '')
		from objects where occurrences > 1
		order by size * (occurrences - 1) desc, size desc, digest limit ?
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

func (d *dedupIndex) RemoveFilesNotIn(paths map[string]struct{}) error {
	if d.tx != nil {
		return errors.New("cannot prune files during active transaction")
	}
	rows, err := d.db.Query(`select path from scanned_files`)
	if err != nil {
		return fmt.Errorf("list indexed files: %w", err)
	}
	var stale []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan indexed file path: %w", err)
		}
		if _, ok := paths[path]; !ok {
			stale = append(stale, path)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	for _, path := range stale {
		if _, err := tx.Exec(`delete from dedup_objects where file_path = ?`, path); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`delete from scanned_files where path = ?`, path); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (d *dedupIndex) Close() error {
	if d == nil || d.closed {
		return nil
	}
	d.closed = true
	if err := d.CommitFile(); err != nil {
		_ = d.db.Close()
		return err
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("close dedup sqlite index: %w", err)
	}
	return nil
}

func normalizeLayerList(layers map[string]bool) string {
	ordered := make([]string, 0, len(layers))
	for _, layer := range []string{LayerCDC, LayerField, LayerRecord} {
		if layers[layer] {
			ordered = append(ordered, layer)
		}
	}
	return strings.Join(ordered, ",")
}
