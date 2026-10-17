package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
)

func TestFSMigrateApplyUsesTargetProofWithUnrelatedUnpackedManifest(t *testing.T) {
	allowFixtureMount(t)
	home, store, native := fsFixture(t, true)
	otherPath := filepath.Join(home, "unrelated.jsonl")
	otherBytes := []byte("{\"type\":\"session_meta\"}\n{\"value\":\"unrelated-unique-unpacked-content\"}\n")
	if err := os.WriteFile(otherPath, otherBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fold.Fold(context.Background(), fold.Session{ID: "unrelated", RolloutPath: otherPath, Archived: true}, fold.FoldOptions{StoreDir: store, Apply: true, FieldThreshold: 8}); err != nil {
		t.Fatal(err)
	}
	report, err := pack.Doctor(context.Background(), store)
	if err != nil || report.IssueCount == 0 {
		t.Fatalf("fixture must have an unrelated unpacked manifest: issues=%d err=%v", report.IssueCount, err)
	}
	mount := filepath.Join(home, "mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mount, "session.jsonl")
	if err := os.WriteFile(target, source, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--apply", "--json", "--codex-home", home, "--store", store, "--mount", mount, "--cli", approvedCLIContract(t, store, "1.2.3"), "--desktop-app", "none"})
	if err := root.Execute(); err != nil {
		t.Fatalf("target migration should use its own complete proof: %v", err)
	}
	var result FSMigrateResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Routed || !result.Shadow.Verified || result.Shadow.ComparedBytes != int64(len(source)) || result.Shadow.RandomReads != 0 {
		t.Fatalf("target was not completely verified: %+v", result)
	}
	if got, err := os.ReadFile(otherPath); err != nil || !bytes.Equal(got, otherBytes) {
		t.Fatalf("unrelated source changed: %v", err)
	}
}

func TestFSMigrateRejectsSameSizeTargetMismatchBeforeMutation(t *testing.T) {
	home, store, native := fsFixture(t, true)
	source, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(source, []byte("repeated"), []byte("modified"), 1)
	if len(changed) != len(source) || bytes.Equal(changed, source) {
		t.Fatal("fixture must change bytes without changing the size")
	}
	if err := os.WriteFile(native, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--apply", "--codex-home", home, "--store", store, "--mount", filepath.Join(home, "mount")})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "shadow byte mismatch") {
		t.Fatalf("same-size source mismatch must stop migration: %v", err)
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || len(sessions) != 1 || sessions[0].RolloutPath != native {
		t.Fatalf("rejected migration changed the route: sessions=%+v err=%v", sessions, err)
	}
	if _, err := os.Stat(filepath.Join(store, "fs", "sessions", "session", "state.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected migration published managed state: %v", err)
	}
}
