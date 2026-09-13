package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestFSMigrateCancellationAfterManagedPublishRecoversWithoutMismatch(t *testing.T) {
	allowFixtureMount(t)
	home := t.TempDir()
	store := filepath.Join(home, "fold-store")
	route := filepath.Join(home, "archived_sessions", "rollout-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(route), 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte("{\"type\":\"session_meta\"}\n{\"cancel_migration\":true}\n")
	if err := os.WriteFile(route, source, 0o600); err != nil {
		t.Fatal(err)
	}
	writeStateFixture(t, home, route)
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, updateErr := db.Exec(`update threads set archived = 1, id = 'session' where id = 'fixture'`)
	if err := errors.Join(updateErr, db.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := fold.Fold(context.Background(), fold.Session{ID: "session", RolloutPath: route, Archived: true}, fold.FoldOptions{StoreDir: store, Apply: true, FieldThreshold: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	manifestPath := fold.ManifestPath(store, "session")
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	nativeRoot := filepath.Join(home, "fold-native")
	nativePath := filepath.Join(nativeRoot, "archived_sessions", filepath.Base(route))
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(route, nativePath); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(home, "fold-fs")
	ctx, cancel := context.WithCancel(context.Background())
	stateObserved := make(chan error, 1)
	go func() {
		statePath := filepath.Join(store, "fs", "sessions", "session", "state.json")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, stateErr := vfs.LoadSessionState(statePath); stateErr == nil {
				cancel()
				stateObserved <- nil
				return
			}
			time.Sleep(time.Millisecond)
		}
		stateObserved <- errors.New("managed state was not published before cancellation")
	}()

	command := NewRootCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{
		"fs", "migrate", "session", "--apply", "--canonical-namespace", "--compatibility-canary",
		"--codex-home", home, "--store", store, "--mount", mount, "--native-root", nativeRoot,
		"--cli", "none", "--desktop-app", "none", "--mount-wait", "5s",
	})
	err = command.ExecuteContext(ctx)
	if observeErr := <-stateObserved; observeErr != nil {
		t.Fatal(observeErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled migrate error = %v, want context.Canceled", err)
	}

	states, err := vfs.DiscoverSessionStates(store)
	if err != nil || len(states) != 1 || states[0].SessionID != "session" {
		t.Fatalf("managed state after cancellation = %#v err=%v", states, err)
	}
	if got, err := os.ReadFile(nativePath); err != nil || !bytes.Equal(got, source) {
		t.Fatalf("native source after cancellation = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(states[0].NativeSnapshot.Path); err != nil || !bytes.Equal(got, source) {
		t.Fatalf("retained snapshot after cancellation = %q err=%v", got, err)
	}
	if report, doctorErr := pack.Doctor(context.Background(), store); doctorErr != nil || report.IssueCount != 0 || report.VerifiedManifestCount != 1 {
		t.Fatalf("pack doctor after canceled migration: report=%#v err=%v", report, doctorErr)
	}
	if report, doctorErr := doctorFoldStore(context.Background(), store); doctorErr != nil || report.IssueCount != 0 || report.VerifiedManifestCount != 1 {
		t.Fatalf("fold doctor after canceled migration: report=%#v err=%v", report, doctorErr)
	}

	recoverCommand := NewRootCommand()
	recoverCommand.SetOut(&bytes.Buffer{})
	recoverCommand.SetErr(&bytes.Buffer{})
	recoverCommand.SetArgs([]string{"fs", "recover", "session", "--apply", "--codex-home", home, "--store", store})
	if err := recoverCommand.Execute(); err != nil {
		t.Fatalf("recover canceled migration: %v", err)
	}
	states, err = vfs.DiscoverSessionStates(store)
	if err != nil || len(states) != 1 || states[0].SessionID != "session" {
		t.Fatalf("managed state after recovery = %#v err=%v", states, err)
	}

	overwrite := NewRootCommand()
	overwrite.SetOut(&bytes.Buffer{})
	overwrite.SetErr(&bytes.Buffer{})
	overwrite.SetArgs([]string{"fold", "session", "--codex-home", home, "--store", store, "--apply", "--overwrite"})
	err = overwrite.Execute()
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite fold manifest for managed session session") {
		t.Fatalf("managed fold overwrite error = %v", err)
	}
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatalf("managed manifest changed after rejected overwrite: equal=%t err=%v", bytes.Equal(manifestAfter, manifestBefore), err)
	}
}
