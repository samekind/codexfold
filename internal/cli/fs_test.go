package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/compat"
	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/fsctl"
	"github.com/jstar0/codexfold/internal/pack"
	"github.com/jstar0/codexfold/internal/vfs"
)

func TestRootExposesPackAndFilesystemCommands(t *testing.T) {
	root := NewRootCommand()
	for _, commandPath := range [][]string{
		{"pack", "build"}, {"pack", "doctor"},
		{"fs", "status"}, {"fs", "doctor"}, {"fs", "compatibility"}, {"fs", "benchmark"},
		{"fs", "serve"}, {"fs", "migrate"}, {"fs", "rollback"}, {"fs", "compact"}, {"fs", "recover"},
		{"fs", "service", "install"}, {"fs", "service", "start"}, {"fs", "service", "stop"},
		{"fs", "service", "status"}, {"fs", "service", "update-preflight"},
	} {
		if _, _, err := root.Find(commandPath); err != nil {
			t.Fatalf("command %v should be exposed: %v", commandPath, err)
		}
	}
}

func TestFSServiceInstallIsDryRunByDefaultAndApplyRequiresFuseBuild(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	plistPath := filepath.Join(home, "LaunchAgents", "com.codexfold.fs.plist")
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "service", "install", "--codex-home", home, "--store", storeDir, "--plist", plistPath, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("service install dry-run: %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote plist: %v", err)
	}
	root = NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "service", "install", "--codex-home", home, "--store", storeDir, "--plist", plistPath, "--apply"})
	if err := root.Execute(); err == nil {
		t.Fatal("default build should reject service installation without a FUSE host")
	}
}

func TestFSUpdatePreflightQuarantineRoutesLatestVisibleBytesNative(t *testing.T) {
	allowFixtureMount(t)
	home, storeDir, nativePath := fsFixture(t, true)
	approvedCLI := approvedCLIContract(t, storeDir, "1.2.3")
	mount := filepath.Join(home, "mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mount, "session.jsonl")
	original, _ := os.ReadFile(nativePath)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	executeFS(t, []string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", mount, "--cli", approvedCLI, "--desktop-app", "none", "--apply"})
	state, err := managedState(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	managed, resolver, err := openManagedSession(context.Background(), storeDir, state)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	tail := []byte("{\"after_upgrade\":true}\n")
	if _, err := writer.Append(context.Background(), tail); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = resolver.Close()
	unknownCLI := fakeCLI(t, "9.9.9")
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "service", "update-preflight", "--codex-home", home, "--store", storeDir, "--cli", unknownCLI, "--desktop-app", "none", "--apply-quarantine", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update preflight: %v", err)
	}
	var result FSUpdatePreflightResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Decision.Quarantine || result.Decision.RequiresNativeFallback || result.QuarantinedSessions != 1 {
		t.Fatalf("unexpected quarantine result: %#v err=%v output=%s", result, err, output.String())
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		t.Fatal(err)
	}
	quarantineBytes, err := os.ReadFile(sessions[0].RolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), original...), tail...)
	if !bytes.Equal(quarantineBytes, want) {
		t.Fatalf("quarantine route is stale: got=%q want=%q", quarantineBytes, want)
	}
}

func TestPackBuildAndDoctorCommandsUseFoldStore(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	var output bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"pack", "build", "--codex-home", home, "--store", storeDir, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("pack build: %v", err)
	}
	var build pack.BuildResult
	if err := json.Unmarshal(output.Bytes(), &build); err != nil || build.ObjectCount == 0 {
		t.Fatalf("unexpected pack build: %#v err=%v output=%s", build, err, output.String())
	}

	output.Reset()
	root = NewRootCommand()
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"pack", "doctor", "--store", storeDir, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("pack doctor: %v", err)
	}
	var doctor pack.DoctorResult
	if err := json.Unmarshal(output.Bytes(), &doctor); err != nil || doctor.IssueCount != 0 {
		t.Fatalf("unexpected pack doctor: %#v err=%v output=%s", doctor, err, output.String())
	}
}

func TestFSMigrateIsDryRunByDefaultAndDoesNotChangeRoute(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount"), "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("fs migrate dry-run: %v", err)
	}
	var result FSMigrateResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.DryRun || result.Routed {
		t.Fatalf("unexpected migrate result: %#v err=%v output=%s", result, err, output.String())
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || sessions[0].RolloutPath != nativePath {
		t.Fatalf("dry-run changed route: sessions=%#v err=%v", sessions, err)
	}
}

func TestFSMigrateApplyFailsClosedWithoutMountedTarget(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "missing-mount"), "--apply"})
	if err := root.Execute(); err == nil {
		t.Fatal("fs migrate --apply should fail without a live mounted target")
	}
	sessions, _ := codex.LoadSessions(home)
	if sessions[0].RolloutPath != nativePath {
		t.Fatalf("failed apply changed route to %q", sessions[0].RolloutPath)
	}
}

func TestFSMigrateApplyRejectsPlainDirectoryThatOnlyLooksLikeMount(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	cliPath := approvedCLIContract(t, storeDir, "1.2.3")
	mount := filepath.Join(home, "plain-directory")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "session.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", mount, "--cli", cliPath, "--desktop-app", "none", "--apply"})
	if err := root.Execute(); err == nil {
		t.Fatal("plain directory should not satisfy the FUSE mount health gate")
	}
	sessions, _ := codex.LoadSessions(home)
	if sessions[0].RolloutPath != nativePath {
		t.Fatalf("failed mount gate changed route to %q", sessions[0].RolloutPath)
	}
}

func TestFSStatusDoesNotClaimTransparentReadiness(t *testing.T) {
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "status", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("fs status: %v", err)
	}
	if bytes.Contains(output.Bytes(), []byte("production-ready")) || bytes.Contains(output.Bytes(), []byte("platform-canary")) {
		t.Fatalf("status overclaimed readiness: %s", output.String())
	}
	var status fsctl.Status
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || status.Capability != fsctl.FSEnginePreview {
		t.Fatalf("status did not report the verified engine preview: %#v err=%v", status, err)
	}
}

func TestFSCompatibilityApprovesOnlyExactInstalledClientContract(t *testing.T) {
	_, storeDir, _ := fsFixture(t, true)
	cliPath := approvedCLIContract(t, storeDir, "1.2.3")
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "compatibility", "--store", storeDir, "--cli", cliPath, "--desktop-app", "none", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("fs compatibility: %v", err)
	}
	var result FSCompatibilityResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Evaluation.Approved || result.Evaluation.Quarantine {
		t.Fatalf("unexpected compatibility result: %#v err=%v output=%s", result, err, output.String())
	}
}

func TestFSMigrateApplyInitializesManagedStateAndRoutesVerifiedTarget(t *testing.T) {
	allowFixtureMount(t)
	home, storeDir, nativePath := fsFixture(t, true)
	cliPath := approvedCLIContract(t, storeDir, "1.2.3")
	mount := filepath.Join(home, "mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mount, "session.jsonl")
	data, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", mount, "--cli", cliPath, "--desktop-app", "none", "--apply"})
	if err := root.Execute(); err != nil {
		t.Fatalf("fs migrate --apply: %v", err)
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || sessions[0].RolloutPath != target {
		t.Fatalf("route not updated: sessions=%#v err=%v", sessions, err)
	}
	states, err := vfs.DiscoverSessionStates(storeDir)
	if err != nil || len(states) != 1 || states[0].NativeSnapshot.Path != nativePath {
		t.Fatalf("managed state missing: %#v err=%v", states, err)
	}
}

func TestFSRollbackUsesLatestVisibleBytesAfterVirtualAppend(t *testing.T) {
	allowFixtureMount(t)
	home, storeDir, nativePath := fsFixture(t, true)
	cliPath := approvedCLIContract(t, storeDir, "1.2.3")
	mount := filepath.Join(home, "mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mount, "session.jsonl")
	original, _ := os.ReadFile(nativePath)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	executeFS(t, []string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", mount, "--cli", cliPath, "--desktop-app", "none", "--apply"})
	state, err := managedState(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	managed, resolver, err := openManagedSession(context.Background(), storeDir, state)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	tail := []byte("{\"appended\":true}\n")
	if _, err := writer.Append(context.Background(), tail); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = resolver.Close()
	executeFS(t, []string{"fs", "rollback", "session", "--codex-home", home, "--store", storeDir, "--apply"})
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := os.ReadFile(sessions[0].RolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), original...), tail...)
	if !bytes.Equal(fallback, want) {
		t.Fatalf("rollback used stale bytes: got=%q want=%q", fallback, want)
	}
}

func TestFSCompactCommitsNewExactGeneration(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	manifest, err := fold.LoadManifest(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := pack.Open(storeDir, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	native, err := hashPath(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest, Reader: resolver, NativeSnapshot: native})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(context.Background(), []byte("{\"tail\":2}\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	before := filepath.Join(home, "before.jsonl")
	expected, err := managed.MaterializeCurrent(context.Background(), before, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = resolver.Close()
	executeFS(t, []string{"fs", "compact", "session", "--codex-home", home, "--store", storeDir, "--apply"})
	state, err := managedState(storeDir, "session")
	if err != nil || state.Generation != 2 || state.BaseSHA256 != expected.SHA256 {
		t.Fatalf("unexpected compacted state: %#v err=%v", state, err)
	}
	reopened, nextResolver, err := openManagedSession(context.Background(), storeDir, state)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.MaterializeCurrent(context.Background(), filepath.Join(home, "after.jsonl"), false)
	_ = nextResolver.Close()
	if err != nil || after.Bytes != expected.Bytes || after.SHA256 != expected.SHA256 {
		t.Fatalf("compacted bytes changed: before=%#v after=%#v err=%v", expected, after, err)
	}
}

func TestFSReadOnlyCommandsRunWithoutClaimingMountHealth(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	for _, args := range [][]string{
		{"fs", "doctor", "--codex-home", home, "--store", storeDir, "--json"},
		{"fs", "benchmark", "session", "--codex-home", home, "--store", storeDir, "--random-reads", "10", "--json"},
		{"fs", "serve", "--codex-home", home, "--store", storeDir, "--json"},
	} {
		root := NewRootCommand()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if bytes.Contains(output.Bytes(), []byte("production-ready")) || bytes.Contains(output.Bytes(), []byte("platform-canary")) {
			t.Fatalf("%v overclaimed readiness: %s", args, output.String())
		}
	}
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "serve", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount"), "--apply"})
	if err := root.Execute(); err == nil {
		t.Fatal("default build should not claim the FUSE prerequisite is available")
	}
	status, _ := fsctl.NewStatus(fsctl.StorageEngine, runtime.GOOS)
	if status.Capability != fsctl.StorageEngine {
		t.Fatalf("unexpected capability: %#v", status)
	}
}

func fsFixture(t *testing.T, archived bool) (string, string, string) {
	t.Helper()
	home := t.TempDir()
	storeDir := filepath.Join(home, "fold-store")
	nativePath := filepath.Join(home, "session.jsonl")
	source := []byte("{\"type\":\"session_meta\"}\n{\"value\":\"repeated-field-value\"}\n")
	if err := os.WriteFile(nativePath, source, 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	writeStateFixture(t, home, nativePath)
	if archived {
		dbPath := filepath.Join(home, "state_5.sqlite")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open state: %v", err)
		}
		_, err = db.Exec(`update threads set archived = 1 where id = 'fixture'; update threads set id = 'session' where id = 'fixture'`)
		_ = db.Close()
		if err != nil {
			t.Fatalf("archive fixture: %v", err)
		}
	}
	session := codex.Session{ID: "session", RolloutPath: nativePath, Archived: archived}
	if _, err := fold.Fold(context.Background(), session, fold.FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 8}); err != nil {
		t.Fatalf("fold fixture: %v", err)
	}
	if _, err := pack.Build(context.Background(), storeDir, pack.BuildOptions{}); err != nil {
		t.Fatalf("pack fixture: %v", err)
	}
	return home, storeDir, nativePath
}

func approvedCLIContract(t *testing.T, storeDir string, version string) string {
	t.Helper()
	cliPath := fakeCLI(t, version)
	_, err := compat.Save(filepath.Join(storeDir, "compatibility"), compat.Contract{
		Version: compat.ContractVersion, Platform: runtime.GOOS, ClientKind: "cli", ClientVersion: version,
		Operations:  []compat.Operation{{Name: "read", Count: 1}},
		TraceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	return cliPath
}

func fakeCLI(t *testing.T, version string) string {
	t.Helper()
	cliPath := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\necho 'codex-cli " + version + "'\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return cliPath
}

func executeFS(t *testing.T, args []string) {
	t.Helper()
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute %v: %v", args, err)
	}
}

func allowFixtureMount(t *testing.T) {
	t.Helper()
	previous := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = previous })
}
