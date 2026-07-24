package scan

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
)

type DuplicateRecordIndex struct {
	database *sql.DB
}

func OpenDuplicateRecordIndex(path string) (*DuplicateRecordIndex, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	var version int
	if err := database.QueryRow(`pragma user_version`).Scan(&version); err != nil {
		_ = database.Close()
		return nil, err
	}
	if version != dedupIndexSchemaVersion {
		_ = database.Close()
		return nil, fmt.Errorf("unsupported scan index schema %d", version)
	}
	if err := prepareDuplicateRecordLookup(database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return &DuplicateRecordIndex{database: database}, nil
}

func prepareDuplicateRecordLookup(database *sql.DB) error {
	if _, err := database.Exec(`create table if not exists duplicate_records (
		digest blob not null,
		size integer not null,
		occurrences integer not null,
		primary key (digest, size)
	) without rowid`); err != nil {
		return fmt.Errorf("create duplicate record lookup: %w", err)
	}
	transaction, err := database.Begin()
	if err != nil {
		return err
	}
	var ready string
	err = transaction.QueryRow(`select value from scan_metadata where key = 'record_lookup_ready'`).Scan(&ready)
	if err == nil && ready == "1" {
		return transaction.Commit()
	}
	if err != nil && err != sql.ErrNoRows {
		_ = transaction.Rollback()
		return err
	}
	if _, err := transaction.Exec(`delete from duplicate_records`); err != nil {
		_ = transaction.Rollback()
		return err
	}
	if _, err := transaction.Exec(`
		insert into duplicate_records(digest, size, occurrences)
		select digest, size, sum(occurrences)
		from dedup_objects where layer = ?
		group by digest, size having sum(occurrences) > 1
	`, LayerRecord); err != nil {
		_ = transaction.Rollback()
		return fmt.Errorf("materialize duplicate record lookup: %w", err)
	}
	if _, err := transaction.Exec(`insert into scan_metadata(key, value) values ('record_lookup_ready', '1') on conflict(key) do update set value = excluded.value`); err != nil {
		_ = transaction.Rollback()
		return err
	}
	return transaction.Commit()
}

func (i *DuplicateRecordIndex) IsDuplicateRecord(ctx context.Context, digest [sha256.Size]byte, size int64) (bool, error) {
	if i == nil || i.database == nil {
		return false, nil
	}
	var occurrences int64
	err := i.database.QueryRowContext(ctx, `select coalesce(occurrences, 0) from duplicate_records where digest = ? and size = ?`, digest[:], size).Scan(&occurrences)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return occurrences >= 2, err
}

func (i *DuplicateRecordIndex) Close() error {
	if i == nil || i.database == nil {
		return nil
	}
	return i.database.Close()
}
