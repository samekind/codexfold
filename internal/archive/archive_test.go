package archive

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	_ "modernc.org/sqlite"
)

func TestArchiveDryRunAndApplyMatchOfficialFileAndStateBehavior(t *testing.T) {
	fixture := archiveFixture(t)
	originalGlobal, err := os.ReadFile(fixture.globalPath)
	if err != nil {
		t.Fatal(err)
	}
	dry, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{Now: fixture.now})
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Archived || dry.TargetPath != filepath.Join(fixture.home, "archived_sessions", filepath.Base(fixture.session.RolloutPath)) {
		t.Fatalf("dry-run result = %#v", dry)
	}
	assertActiveSource(t, fixture)

	result, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{
		Apply: true, Now: fixture.now, WriterActive: idleWriter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DryRun || !result.Archived || result.Bytes != int64(len(fixture.source)) || result.SHA256 == "" {
		t.Fatalf("archive result = %#v", result)
	}
	if _, err := os.Lstat(fixture.session.RolloutPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active source remains after archive: %v", err)
	}
	archived, err := os.ReadFile(result.TargetPath)
	if err != nil || string(archived) != string(fixture.source) {
		t.Fatalf("archived bytes changed: %q err=%v", archived, err)
	}
	var path string
	var archivedFlag int
	var archivedAt sql.NullInt64
	var updatedAt int64
	var updatedAtMillis int64
	if err := fixture.db.QueryRow(`select rollout_path, archived, archived_at, updated_at, updated_at_ms from threads where id = ?`, fixture.session.ID).Scan(
		&path, &archivedFlag, &archivedAt, &updatedAt, &updatedAtMillis,
	); err != nil {
		t.Fatal(err)
	}
	if path != result.TargetPath || archivedFlag != 1 || !archivedAt.Valid || archivedAt.Int64 != fixture.now.Unix() || updatedAt != 100 || updatedAtMillis != 301 {
		t.Fatalf("archived database row = path=%s archived=%d archived_at=%#v updated=%d/%d", path, archivedFlag, archivedAt, updatedAt, updatedAtMillis)
	}
	afterGlobal, err := os.ReadFile(fixture.globalPath)
	if err != nil || string(afterGlobal) != string(originalGlobal) {
		t.Fatalf("archive changed global state: %q err=%v", afterGlobal, err)
	}
	if _, err := os.Lstat(JournalPath(fixture.store, fixture.session.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed archive left a journal: %v", err)
	}
}

func TestArchiveApplyRequiresWriterProbe(t *testing.T) {
	fixture := archiveFixture(t)
	_, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{Apply: true})
	if err == nil {
		t.Fatal("archive apply accepted a missing writer probe")
	}
	assertActiveSource(t, fixture)
}

func TestArchiveRejectsWriterRouteChangeAndSourceMutationBeforeRename(t *testing.T) {
	t.Run("writer", func(t *testing.T) {
		fixture := archiveFixture(t)
		_, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{
			Apply: true,
			WriterActive: func(context.Context, codex.Session) (bool, error) {
				return true, nil
			},
		})
		if err == nil {
			t.Fatal("active writer was not rejected")
		}
		assertActiveSource(t, fixture)
	})

	t.Run("route change", func(t *testing.T) {
		fixture := archiveFixture(t)
		_, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{
			Apply: true, WriterActive: idleWriter,
			BeforeRename: func() error {
				_, err := fixture.db.Exec(`update threads set rollout_path = ? where id = ?`, filepath.Join(fixture.home, "other.jsonl"), fixture.session.ID)
				return err
			},
		})
		if err == nil {
			t.Fatal("concurrent route change was not rejected")
		}
		if data, readErr := os.ReadFile(fixture.session.RolloutPath); readErr != nil || string(data) != string(fixture.source) {
			t.Fatalf("route-race source changed: %q err=%v", data, readErr)
		}
	})

	t.Run("source mutation", func(t *testing.T) {
		fixture := archiveFixture(t)
		_, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{
			Apply: true, WriterActive: idleWriter,
			BeforeRename: func() error {
				return os.WriteFile(fixture.session.RolloutPath, append(append([]byte(nil), fixture.source...), []byte("{\"changed\":true}\n")...), 0o600)
			},
		})
		if err == nil {
			t.Fatal("concurrent source mutation was not rejected")
		}
		var archived int
		if dbErr := fixture.db.QueryRow(`select archived from threads where id = ?`, fixture.session.ID).Scan(&archived); dbErr != nil || archived != 0 {
			t.Fatalf("source-race database changed: archived=%d err=%v", archived, dbErr)
		}
	})
}

func TestArchiveFailureAfterRenameRollsBackFileDatabaseAndJournal(t *testing.T) {
	fixture := archiveFixture(t)
	_, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{
		Apply: true, WriterActive: idleWriter,
		AfterRename: func() error {
			return errors.New("injected failure")
		},
	})
	if err == nil {
		t.Fatal("injected archive failure returned nil")
	}
	assertActiveSource(t, fixture)
	if _, err := os.Lstat(filepath.Join(fixture.home, "archived_sessions", filepath.Base(fixture.session.RolloutPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed archive left target: %v", err)
	}
	if _, err := os.Lstat(JournalPath(fixture.store, fixture.session.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back archive left journal: %v", err)
	}
}

func TestArchiveCommitFailureDoesNotGuessTransactionOutcome(t *testing.T) {
	originalCommit := commitArchiveTransaction
	t.Cleanup(func() { commitArchiveTransaction = originalCommit })

	t.Run("not committed rolls back", func(t *testing.T) {
		fixture := archiveFixture(t)
		commitArchiveTransaction = func(context.Context, *sql.Conn) error {
			return errors.New("injected commit failure")
		}
		_, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{
			Apply: true, WriterActive: idleWriter,
		})
		if err == nil {
			t.Fatal("commit failure returned nil")
		}
		assertActiveSource(t, fixture)
		if _, statErr := os.Lstat(JournalPath(fixture.store, fixture.session.ID)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rolled-back commit failure left journal: %v", statErr)
		}
	})

	t.Run("committed but acknowledgement failed leaves recovery journal", func(t *testing.T) {
		fixture := archiveFixture(t)
		commitArchiveTransaction = func(ctx context.Context, conn *sql.Conn) error {
			if _, err := conn.ExecContext(ctx, `commit`); err != nil {
				return err
			}
			return errors.New("lost commit acknowledgement")
		}
		result, err := Archive(context.Background(), fixture.home, fixture.store, fixture.session, Options{
			Apply: true, WriterActive: idleWriter,
		})
		if err == nil {
			t.Fatal("ambiguous commit acknowledgement returned nil")
		}
		if _, statErr := os.Lstat(result.TargetPath); statErr != nil {
			t.Fatalf("committed archive target missing: %v", statErr)
		}
		if _, statErr := os.Lstat(JournalPath(fixture.store, fixture.session.ID)); statErr != nil {
			t.Fatalf("ambiguous commit did not retain recovery journal: %v", statErr)
		}
		recovered, recoverErr := Recover(context.Background(), fixture.home, fixture.store, fixture.session.ID)
		if recoverErr != nil || !recovered.Finalized || recovered.RolledBack {
			t.Fatalf("recover committed archive = %#v err=%v", recovered, recoverErr)
		}
	})
}

func TestRecoverRollsBackRenamedFileOrFinalizesCommittedState(t *testing.T) {
	t.Run("rollback", func(t *testing.T) {
		fixture := archiveFixture(t)
		target := filepath.Join(fixture.home, "archived_sessions", filepath.Base(fixture.session.RolloutPath))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(fixture.session.RolloutPath, target); err != nil {
			t.Fatal(err)
		}
		if err := writeJournal(JournalPath(fixture.store, fixture.session.ID), journal{
			Version: 1, SessionID: fixture.session.ID, SourcePath: fixture.session.RolloutPath, TargetPath: target,
			Bytes: int64(len(fixture.source)), SHA256: hashBytes(fixture.source), Phase: phaseRenamed,
		}); err != nil {
			t.Fatal(err)
		}
		result, err := Recover(context.Background(), fixture.home, fixture.store, fixture.session.ID)
		if err != nil || !result.RolledBack || result.Finalized {
			t.Fatalf("rollback recovery = %#v err=%v", result, err)
		}
		assertActiveSource(t, fixture)
	})

	t.Run("finalize", func(t *testing.T) {
		fixture := archiveFixture(t)
		target := filepath.Join(fixture.home, "archived_sessions", filepath.Base(fixture.session.RolloutPath))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(fixture.session.RolloutPath, target); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.db.Exec(`update threads set rollout_path = ?, archived = 1, archived_at = ? where id = ?`, target, fixture.now.Unix(), fixture.session.ID); err != nil {
			t.Fatal(err)
		}
		if err := writeJournal(JournalPath(fixture.store, fixture.session.ID), journal{
			Version: 1, SessionID: fixture.session.ID, SourcePath: fixture.session.RolloutPath, TargetPath: target,
			Bytes: int64(len(fixture.source)), SHA256: hashBytes(fixture.source), Phase: phaseRenamed,
		}); err != nil {
			t.Fatal(err)
		}
		result, err := Recover(context.Background(), fixture.home, fixture.store, fixture.session.ID)
		if err != nil || result.RolledBack || !result.Finalized {
			t.Fatalf("finalize recovery = %#v err=%v", result, err)
		}
		if data, readErr := os.ReadFile(target); readErr != nil || string(data) != string(fixture.source) {
			t.Fatalf("finalized target changed: %q err=%v", data, readErr)
		}
	})
}

func TestRecoverRejectsUnsafeIdentityAndJournalPaths(t *testing.T) {
	fixture := archiveFixture(t)
	if _, err := Recover(context.Background(), fixture.home, fixture.store, "../session"); err == nil {
		t.Fatal("recovery accepted an unsafe session ID")
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, fixture.source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(JournalPath(fixture.store, fixture.session.ID), journal{
		Version: 1, SessionID: fixture.session.ID, SourcePath: outside,
		TargetPath: filepath.Join(fixture.home, "archived_sessions", filepath.Base(outside)),
		Bytes:      int64(len(fixture.source)), SHA256: hashBytes(fixture.source), Phase: phasePrepared,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(context.Background(), fixture.home, fixture.store, fixture.session.ID); err == nil {
		t.Fatal("recovery accepted a journal outside the active sessions tree")
	}
}

type fixture struct {
	home       string
	store      string
	db         *sql.DB
	session    codex.Session
	source     []byte
	globalPath string
	now        time.Time
}

func archiveFixture(t *testing.T) fixture {
	t.Helper()
	home := t.TempDir()
	store := filepath.Join(home, "fold-store")
	rollout := filepath.Join(home, "sessions", "2026", "07", "16", "rollout-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte("{\"type\":\"session_meta\"}\n{\"value\":1}\n")
	if err := os.WriteFile(rollout, source, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		create table threads (
			id text primary key,
			rollout_path text not null,
			archived integer not null,
			archived_at integer,
			updated_at integer not null,
			updated_at_ms integer not null
		);
		insert into threads values ('session', ?, 0, null, 100, 200);
		insert into threads values ('newer', '/tmp/newer.jsonl', 0, null, 300, 300);
	`, rollout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	globalPath := filepath.Join(home, ".codex-global-state.json")
	if err := os.WriteFile(globalPath, []byte("{\"selectedThreadId\":\"session\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture{
		home: home, store: store, db: db,
		session: codex.Session{ID: "session", RolloutPath: rollout},
		source:  source, globalPath: globalPath, now: time.Unix(1_800_000_000, 0),
	}
}

func assertActiveSource(t *testing.T, fixture fixture) {
	t.Helper()
	data, err := os.ReadFile(fixture.session.RolloutPath)
	if err != nil || string(data) != string(fixture.source) {
		t.Fatalf("active source = %q err=%v", data, err)
	}
	var path string
	var archived int
	if err := fixture.db.QueryRow(`select rollout_path, archived from threads where id = ?`, fixture.session.ID).Scan(&path, &archived); err != nil {
		t.Fatal(err)
	}
	if path != fixture.session.RolloutPath || archived != 0 {
		t.Fatalf("active row = path=%s archived=%d", path, archived)
	}
}

func idleWriter(context.Context, codex.Session) (bool, error) {
	return false, nil
}
