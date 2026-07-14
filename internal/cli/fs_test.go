package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/compat"
	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/fsctl"
	"github.com/jstar0/codexfold/internal/mountfs"
	"github.com/jstar0/codexfold/internal/pack"
	"github.com/jstar0/codexfold/internal/vfs"
)

func TestRootExposesPackAndFilesystemCommands(t *testing.T) {
	root := NewRootCommand()
	for _, commandPath := range [][]string{
		{"pack", "build"}, {"pack", "doctor"},
		{"fs", "status"}, {"fs", "doctor"}, {"fs", "compatibility"}, {"fs", "compatibility-import"}, {"fs", "benchmark"},
		{"fs", "serve"}, {"fs", "migrate"}, {"fs", "rollback"}, {"fs", "compact"}, {"fs", "recover"},
		{"fs", "namespace", "status"}, {"fs", "namespace", "activate"},
		{"fs", "namespace", "deactivate"}, {"fs", "namespace", "recover"},
		{"fs", "service", "install"}, {"fs", "service", "start"}, {"fs", "service", "stop"},
		{"fs", "service", "status"}, {"fs", "service", "update-preflight"},
	} {
		if _, _, err := root.Find(commandPath); err != nil {
			t.Fatalf("command %v should be exposed: %v", commandPath, err)
		}
	}
}

func TestFSCompatibilityImportPersistsOnlySanitizedContract(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, "fold-store")
	trace := filepath.Join(home, "private-trace.log")
	traceText := "12:00:00 open /Users/private/.codex/secret.jsonl codex.1\n12:00:01 read /Users/private/.codex/secret.jsonl codex.1\n"
	if err := os.WriteFile(trace, []byte(traceText), 0o600); err != nil {
		t.Fatal(err)
	}
	executeFS(t, []string{
		"fs", "compatibility-import", "--apply", "--codex-home", home, "--store", store,
		"--trace", trace, "--client-kind", "cli", "--client-version", "1.2.3",
	})
	contracts, err := compat.LoadAll(filepath.Join(store, "compatibility"))
	if err != nil || len(contracts) != 1 || contracts[0].ClientVersion != "1.2.3" {
		t.Fatalf("contracts = %#v err=%v", contracts, err)
	}
	data, err := os.ReadFile(filepath.Join(store, "compatibility", runtime.GOOS, "cli", "1.2.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("/Users/private")) || bytes.Contains(data, []byte("secret.jsonl")) {
		t.Fatalf("sanitized contract leaked trace content: %s", data)
	}
}

func TestOperationRecorderWritesOnlyTimeAndOperation(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "operations.log")
	record, closer, err := newOperationRecorder(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	record("open")
	record("read")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("/")) || !bytes.Contains(data, []byte(" open\n")) || !bytes.Contains(data, []byte(" read\n")) {
		t.Fatalf("operation trace = %q", data)
	}
}

func TestFSNamespaceActivateAndDeactivateCommandsPreserveNativeFiles(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mount := filepath.Join(root, "mount")
	nativeRoot := filepath.Join(home, "fold-native")
	configPath := filepath.Join(home, "config.toml")
	authPath := filepath.Join(home, "auth.json")
	configBefore := []byte("model_provider = \"third-party\"\n")
	authBefore := []byte("{\"access_token\":\"test\"}\n")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, authBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(home, "sessions", "active.jsonl"),
		filepath.Join(home, "archived_sessions", "archived.jsonl"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(mount, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	database, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`create table threads (id text primary key, rollout_path text not null)`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	previousProbe := mountHealthProbe
	t.Cleanup(func() { mountHealthProbe = previousProbe })
	mountHealthProbe = func(string) error { return nil }
	executeFS(t, []string{
		"fs", "namespace", "activate", "--apply",
		"--codex-home", home, "--mount", mount, "--native-root", nativeRoot,
	})
	for _, directory := range []string{"sessions", "archived_sessions"} {
		if target, err := os.Readlink(filepath.Join(home, directory)); err != nil || filepath.Clean(target) != filepath.Join(mount, directory) {
			t.Fatalf("namespace link %s = %q err=%v", directory, target, err)
		}
	}
	mountHealthProbe = func(string) error { return errors.New("not mounted") }
	executeFS(t, []string{
		"fs", "namespace", "deactivate", "--apply",
		"--codex-home", home, "--mount", mount, "--native-root", nativeRoot,
	})
	for _, path := range []string{
		filepath.Join(home, "sessions", "active.jsonl"),
		filepath.Join(home, "archived_sessions", "archived.jsonl"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("restored file %s: %v", path, err)
		}
	}
	if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, configBefore) {
		t.Fatalf("config.toml changed during namespace lifecycle: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(authPath); err != nil || !bytes.Equal(got, authBefore) {
		t.Fatalf("auth.json changed during namespace lifecycle: %q err=%v", got, err)
	}
}

func TestFSNamespaceDeactivateRejectsManagedSessions(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mount := filepath.Join(root, "mount")
	nativeRoot := filepath.Join(home, "fold-native")
	store := filepath.Join(home, "fold-store")
	for _, directory := range []string{
		filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions"),
		filepath.Join(mount, "sessions"), filepath.Join(mount, "archived_sessions"),
		filepath.Join(nativeRoot, "sessions"), filepath.Join(nativeRoot, "archived_sessions"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stateDirectory := filepath.Join(store, "fs", "sessions", "managed")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	state := vfs.SessionState{
		Version: 1, SessionID: "managed", Generation: 1,
		ManifestPath: filepath.Join(store, "manifests", "managed.json"),
		BaseSHA256:   "0000000000000000000000000000000000000000000000000000000000000000",
		DeltaPath:    filepath.Join(stateDirectory, "delta.jsonl"),
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirectory, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	previousProbe := mountHealthProbe
	t.Cleanup(func() { mountHealthProbe = previousProbe })
	mountHealthProbe = func(string) error { return errors.New("not mounted") }
	command := NewRootCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{
		"fs", "namespace", "deactivate", "--apply",
		"--codex-home", home, "--store", store, "--mount", mount, "--native-root", nativeRoot,
	})
	err = command.Execute()
	if err == nil || err.Error() != "rollback all managed sessions before deactivating the namespace" {
		t.Fatalf("deactivate error = %v", err)
	}
}

func TestCanonicalMountRouteMirrorsCodexSessionNamespace(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "tmp", "codex-home")
	mount := filepath.Join(string(filepath.Separator), "tmp", "codex-fold")
	route := filepath.Join(home, "archived_sessions", "rollout-session.jsonl")
	got, err := canonicalMountRoute(home, mount, route)
	if err != nil || got != filepath.Join(mount, "archived_sessions", "rollout-session.jsonl") {
		t.Fatalf("canonicalMountRoute = %q err=%v", got, err)
	}
	if _, err := canonicalMountRoute(home, mount, filepath.Join(home, "other", "rollout.jsonl")); err == nil {
		t.Fatal("non-canonical Codex route should be rejected")
	}
}

func TestCanonicalSessionRoutesIgnoreUnmanagedSessionsOutsideCodexHome(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "tmp", "codex-home")
	mount := filepath.Join(home, "fold-fs")
	states := []vfs.SessionState{{SessionID: "managed"}}
	sessions := []codex.Session{
		{ID: "managed", RolloutPath: filepath.Join(home, "sessions", "2026", "07", "12", "rollout-managed.jsonl")},
		{ID: "unmanaged", RolloutPath: filepath.Join(string(filepath.Separator), "tmp", "native-fallback.jsonl")},
	}
	routes, err := canonicalSessionRoutes(home, mount, filepath.Join(home, "fold-store"), states, sessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes["managed"] != "/sessions/2026/07/12/rollout-managed.jsonl" {
		t.Fatalf("canonical routes = %#v", routes)
	}
}

func TestCanonicalSessionRoutesSkipManagedNativeFallback(t *testing.T) {
	home := t.TempDir()
	mount := filepath.Join(home, "fold-fs")
	store := filepath.Join(home, "fold-store")
	state := vfs.SessionState{SessionID: "session"}
	fallback := filepath.Join(store, "fs", "sessions", "session", "quarantine-current.jsonl")
	routes, err := canonicalSessionRoutes(home, mount, store, []vfs.SessionState{state}, []codex.Session{{ID: "session", RolloutPath: fallback}})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("native fallback leaked into canonical routes: %#v", routes)
	}
}

func TestCanonicalSessionRoutesAcceptDesktopMountAlias(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "tmp", "codex-home")
	mount := filepath.Join(home, "fold-fs")
	states := []vfs.SessionState{{SessionID: "managed"}}
	sessions := []codex.Session{{
		ID:          "managed",
		RolloutPath: filepath.Join(mount, "sessions", "2026", "07", "13", "rollout-managed.jsonl"),
	}}
	routes, err := canonicalSessionRoutes(home, mount, filepath.Join(home, "fold-store"), states, sessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes["managed"] != "/sessions/2026/07/13/rollout-managed.jsonl" {
		t.Fatalf("canonical routes from mount alias = %#v", routes)
	}
}

func TestDiscoverCanonicalRoutesSkipsCodexDatabaseWhenNoSessionsAreManaged(t *testing.T) {
	called := false
	routes, err := discoverCanonicalRoutes("/tmp/codex-home", "/tmp/codex-home/fold-fs", "/tmp/store", nil, func(string) ([]codex.Session, error) {
		called = true
		return nil, errors.New("database should not be opened")
	})
	if err != nil || called || len(routes) != 0 {
		t.Fatalf("empty canonical routes = %#v called=%t err=%v", routes, called, err)
	}
}

func TestCanonicalFSServeRequiresAbsoluteNativeRoot(t *testing.T) {
	home := t.TempDir()
	for _, nativeRoot := range []string{"", "relative-native-root"} {
		root := NewRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{
			"fs", "serve",
			"--canonical-namespace",
			"--native-root", nativeRoot,
			"--codex-home", home,
		})
		if err := root.Execute(); err == nil {
			t.Fatalf("native root %q should be rejected", nativeRoot)
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
	if mountfs.Available() {
		return
	}
	root = NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "service", "install", "--codex-home", home, "--store", storeDir, "--plist", plistPath, "--apply"})
	err := root.Execute()
	if err == nil {
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
	if _, err := managedState(storeDir, "session"); err == nil {
		t.Fatal("quarantine left the session managed")
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

func TestCompatibilityCanaryRequiresIsolatedCanonicalHomeAndSkippedClients(t *testing.T) {
	defaultHome := filepath.Join(t.TempDir(), ".codex")
	isolatedHome := filepath.Join(t.TempDir(), "isolated")
	isolatedStore := filepath.Join(isolatedHome, "fold-store")
	skipped := compatibilityFlags{cliPath: "none", desktopPath: "none"}
	if err := validateCompatibilityCanary(isolatedHome, defaultHome, isolatedStore, true, skipped); err != nil {
		t.Fatalf("isolated canonical canary was rejected: %v", err)
	}
	for _, test := range []struct {
		name      string
		home      string
		store     string
		canonical bool
		flags     compatibilityFlags
	}{
		{name: "real home", home: defaultHome, store: filepath.Join(defaultHome, "fold-store"), canonical: true, flags: skipped},
		{name: "external store", home: isolatedHome, store: filepath.Join(t.TempDir(), "store"), canonical: true, flags: skipped},
		{name: "flat mount", home: isolatedHome, store: isolatedStore, canonical: false, flags: skipped},
		{name: "live cli", home: isolatedHome, store: isolatedStore, canonical: true, flags: compatibilityFlags{cliPath: "codex", desktopPath: "none"}},
		{name: "live desktop", home: isolatedHome, store: isolatedStore, canonical: true, flags: compatibilityFlags{cliPath: "none", desktopPath: "/Applications/ChatGPT.app"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCompatibilityCanary(test.home, defaultHome, test.store, test.canonical, test.flags); err == nil {
				t.Fatal("unsafe compatibility canary configuration was accepted")
			}
		})
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

func TestFSMigrateCanonicalKeepsCodexRouteAndHidesRetainedSnapshot(t *testing.T) {
	allowFixtureMount(t)
	home := t.TempDir()
	storeDir := filepath.Join(home, "fold-store")
	route := filepath.Join(home, "archived_sessions", "rollout-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(route), 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte("{\"type\":\"session_meta\"}\n{\"canonical\":true}\n")
	if err := os.WriteFile(route, source, 0o600); err != nil {
		t.Fatal(err)
	}
	writeStateFixture(t, home, route)
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`update threads set archived = 1, id = 'session' where id = 'fixture'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := fold.Fold(context.Background(), codex.Session{ID: "session", RolloutPath: route, Archived: true}, fold.FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Build(context.Background(), storeDir, pack.BuildOptions{}); err != nil {
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
	target := filepath.Join(mount, "archived_sessions", filepath.Base(route))
	cliPath := approvedCLIContract(t, storeDir, "1.2.3")
	acknowledged := make(chan error, 1)
	go func() {
		statePath := filepath.Join(storeDir, "fs", "sessions", "session", "state.json")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			state, err := vfs.LoadSessionState(statePath)
			if err == nil {
				if err := writeMountAcknowledgement(storeDir, "session", state.Generation, "/archived_sessions/"+filepath.Base(route)); err != nil {
					acknowledged <- err
					return
				}
				time.Sleep(100 * time.Millisecond)
				if _, err := os.Stat(nativePath); err != nil {
					acknowledged <- errors.New("canonical source was hidden before mounted target verification")
					return
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err == nil {
					err = os.WriteFile(target, source, 0o600)
				}
				acknowledged <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		acknowledged <- errors.New("managed state was not created")
	}()
	executeFS(t, []string{
		"fs", "migrate", "session", "--apply", "--canonical-namespace",
		"--codex-home", home, "--store", storeDir, "--mount", mount, "--native-root", nativeRoot,
		"--cli", cliPath, "--desktop-app", "none", "--mount-wait", "500ms",
	})
	if err := <-acknowledged; err != nil {
		t.Fatal(err)
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || len(sessions) != 1 || filepath.Clean(sessions[0].RolloutPath) != filepath.Clean(route) {
		t.Fatalf("canonical migration changed Codex route: sessions=%#v err=%v", sessions, err)
	}
	states, err := vfs.DiscoverSessionStates(storeDir)
	retainedPath := filepath.Join(storeDir, "fs", "snapshots", "session", "native.jsonl")
	if err != nil || len(states) != 1 || filepath.Clean(states[0].NativeSnapshot.Path) != filepath.Clean(retainedPath) {
		t.Fatalf("canonical native snapshot = %#v err=%v", states, err)
	}
	if _, err := os.Stat(nativePath); !os.IsNotExist(err) {
		t.Fatalf("canonical source remained visible after migration: %v", err)
	}
	if got, err := os.ReadFile(retainedPath); err != nil || !bytes.Equal(got, source) {
		t.Fatalf("hidden retained snapshot = %q err=%v", got, err)
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
	if _, err := managedState(storeDir, "session"); err == nil {
		t.Fatal("rollback left the session managed")
	}
}

func TestFSRollbackRejectsActiveWriter(t *testing.T) {
	home, storeDir, originalPath := fsFixture(t, true)
	manifest, err := fold.LoadManifest(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := pack.Open(storeDir, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	native, err := hashPath(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest,
		Reader: resolver, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "rollback", "session", "--codex-home", home, "--store", storeDir, "--apply"})
	err = root.Execute()
	if err == nil || !strings.Contains(err.Error(), "active writer") {
		t.Fatalf("rollback error = %v, want active writer rejection", err)
	}
	if _, err := managedState(storeDir, "session"); err != nil {
		t.Fatalf("active-writer rejection retired managed state: %v", err)
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || len(sessions) != 1 || filepath.Clean(sessions[0].RolloutPath) != filepath.Clean(originalPath) {
		t.Fatalf("active-writer rejection changed route: sessions=%#v err=%v", sessions, err)
	}
}

func TestFSRollbackCanonicalRetiresManagedStateAndKeepsRoute(t *testing.T) {
	allowFixtureMount(t)
	home, storeDir, originalPath := fsFixture(t, true)
	original, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	filename := "rollout-session.jsonl"
	snapshotRoute := filepath.Join(home, "archived_sessions", filename)
	route := filepath.Join(home, "sessions", "2026", "07", "12", filename)
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`update threads set rollout_path = ? where id = 'session'`, route); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	nativeRoot := filepath.Join(home, "fold-native")
	nativePath := filepath.Join(nativeRoot, "archived_sessions", filename)
	targetNativePath := filepath.Join(nativeRoot, "sessions", "2026", "07", "12", filename)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalPath, nativePath); err != nil {
		t.Fatal(err)
	}
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
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest,
		Reader: resolver, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	tail := []byte("{\"canonical_rollback\":true}\n")
	if _, err := writer.Append(context.Background(), tail); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = resolver.Close()
	mount := filepath.Join(home, "fold-fs")
	mountedTarget := filepath.Join(mount, "sessions", "2026", "07", "12", filename)
	if err := os.MkdirAll(filepath.Dir(mountedTarget), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mountedTarget, original, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Join(storeDir, "fs", "sessions", "session")
	copyDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(stateDirectory); os.IsNotExist(err) {
				data, readErr := os.ReadFile(targetNativePath)
				if readErr == nil {
					readErr = os.WriteFile(mountedTarget, data, 0o600)
				}
				copyDone <- readErr
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		copyDone <- errors.New("managed state was not retired")
	}()
	executeFS(t, []string{
		"fs", "rollback", "session", "--apply", "--canonical-namespace",
		"--codex-home", home, "--store", storeDir, "--mount", mount, "--native-root", nativeRoot,
	})
	if err := <-copyDone; err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), original...), tail...)
	if got, err := os.ReadFile(targetNativePath); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("canonical rollback bytes = %q err=%v", got, err)
	}
	if _, err := os.Stat(nativePath); !os.IsNotExist(err) {
		t.Fatalf("retained snapshot remained visible at %s: %v", snapshotRoute, err)
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || len(sessions) != 1 || filepath.Clean(sessions[0].RolloutPath) != filepath.Clean(route) {
		t.Fatalf("canonical rollback changed route: sessions=%#v err=%v", sessions, err)
	}
	if _, err := os.Stat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("managed state remained after canonical rollback: %v", err)
	}
	retired, err := filepath.Glob(filepath.Join(storeDir, "fs", "retired", "session-*"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("retired state = %#v err=%v", retired, err)
	}
	retained, err := filepath.Glob(filepath.Join(retired[0], "retained-native", "archived_sessions", filename))
	if err != nil || len(retained) != 1 {
		t.Fatalf("retired native snapshot = %#v err=%v", retained, err)
	}
}

func TestFSRollbackCanonicalRetiresHiddenSnapshot(t *testing.T) {
	allowFixtureMount(t)
	home, storeDir, originalPath := fsFixture(t, true)
	original, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	filename := "rollout-session.jsonl"
	route := filepath.Join(home, "sessions", "2026", "07", "12", filename)
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`update threads set rollout_path = ? where id = 'session'`, route); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	nativeRoot := filepath.Join(home, "fold-native")
	nativePath := filepath.Join(nativeRoot, "archived_sessions", filename)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalPath, nativePath); err != nil {
		t.Fatal(err)
	}
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
		_ = resolver.Close()
		t.Fatal(err)
	}
	hiddenPath := filepath.Join(storeDir, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenPath), 0o700); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := os.Rename(nativePath, hiddenPath); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	native.Path = hiddenPath
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest,
		Reader: resolver, NativeSnapshot: native,
	})
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	tail := []byte("{\"hidden_snapshot_rollback\":true}\n")
	if _, err := writer.Append(context.Background(), tail); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = resolver.Close()
	mount := filepath.Join(home, "fold-fs")
	mountedTarget := filepath.Join(mount, "sessions", "2026", "07", "12", filename)
	targetNativePath := filepath.Join(nativeRoot, "sessions", "2026", "07", "12", filename)
	if err := os.MkdirAll(filepath.Dir(mountedTarget), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mountedTarget, original, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Join(storeDir, "fs", "sessions", "session")
	copyDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(stateDirectory); os.IsNotExist(err) {
				data, readErr := os.ReadFile(targetNativePath)
				if readErr == nil {
					readErr = os.WriteFile(mountedTarget, data, 0o600)
				}
				copyDone <- readErr
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		copyDone <- errors.New("managed state was not retired")
	}()
	// The mounted target is only used as the FUSE visibility probe. The
	// canonical rollback writes the latest bytes to the retained native route.
	executeFS(t, []string{
		"fs", "rollback", "session", "--apply", "--canonical-namespace",
		"--codex-home", home, "--store", storeDir, "--mount", mount, "--native-root", nativeRoot,
	})
	if err := <-copyDone; err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), original...), tail...)
	retired, err := filepath.Glob(filepath.Join(storeDir, "fs", "retired", "session-*", "retained-native", "store-snapshot", "native.jsonl"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("hidden snapshot retirement = %#v err=%v", retired, err)
	}
	if got, err := os.ReadFile(retired[0]); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("retired hidden snapshot bytes = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(targetNativePath); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("canonical rollback bytes = %q err=%v", got, err)
	}
	if _, err := os.Stat(hiddenPath); !os.IsNotExist(err) {
		t.Fatalf("hidden snapshot remained after retirement: %v", err)
	}
	if _, err := os.Stat(nativePath); !os.IsNotExist(err) {
		t.Fatalf("legacy native snapshot unexpectedly restored: %v", err)
	}
}

func TestFSUpdatePreflightPreservesNewerNativeFallbackAfterRollback(t *testing.T) {
	allowFixtureMount(t)
	home, storeDir, nativePath := fsFixture(t, true)
	cliPath := approvedCLIContract(t, storeDir, "1.2.3")
	mount := filepath.Join(home, "mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mount, "session.jsonl")
	original, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
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
		_ = resolver.Close()
		t.Fatal(err)
	}
	managedTail := []byte("{\"managed_tail\":true}\n")
	if _, err := writer.Append(context.Background(), managedTail); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = resolver.Close()

	executeFS(t, []string{"fs", "rollback", "session", "--codex-home", home, "--store", storeDir, "--apply"})
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		t.Fatal(err)
	}
	fallbackPath := sessions[0].RolloutPath
	if filepath.Base(fallbackPath) != "fallback-current.jsonl" {
		t.Fatalf("rollback did not use the generated fallback: %s", fallbackPath)
	}

	nativeTail := []byte("{\"native_tail\":true}\n")
	fallback, err := os.OpenFile(fallbackPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fallback.Write(nativeTail); err != nil {
		_ = fallback.Close()
		t.Fatal(err)
	}
	if err := fallback.Sync(); err != nil {
		_ = fallback.Close()
		t.Fatal(err)
	}
	if err := fallback.Close(); err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), original...), managedTail...), nativeTail...)
	beforeRoute := fallbackPath

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
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode preflight result: %v output=%s", err, output.String())
	}
	if !result.Decision.Quarantine || result.Decision.RequiresNativeFallback || result.QuarantinedSessions != 0 {
		t.Fatalf("unexpected fallback preflight result: %#v output=%s", result, output.String())
	}
	sessions, err = codex.LoadSessions(home)
	if err != nil {
		t.Fatal(err)
	}
	if sessions[0].RolloutPath != beforeRoute {
		t.Fatalf("preflight replaced newer native fallback: got=%s want=%s", sessions[0].RolloutPath, beforeRoute)
	}
	got, err := os.ReadFile(fallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("preflight changed newer native fallback: got=%q want=%q", got, want)
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
	if !mountfs.Available() {
		root := NewRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"fs", "serve", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount"), "--apply"})
		if err := root.Execute(); err == nil {
			t.Fatal("default build should not claim the FUSE prerequisite is available")
		}
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
