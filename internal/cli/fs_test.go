package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/compat"
	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/fsctl"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

type cliRejectingChecker struct {
	Calls      int
	Projection storage.Projection
}

func (c *cliRejectingChecker) Check(_ context.Context, projection storage.Projection) (storage.Assessment, error) {
	c.Calls++
	c.Projection = projection
	return storage.Assessment{}, storage.ErrBudgetExceeded
}

func TestRootExposesPackAndFilesystemCommands(t *testing.T) {
	root := NewRootCommand()
	for _, commandPath := range [][]string{
		{"pack", "build"}, {"pack", "doctor"},
		{"fs", "status"}, {"fs", "doctor"}, {"fs", "validate-native"}, {"fs", "compatibility"}, {"fs", "compatibility-import"}, {"fs", "benchmark"},
		{"fs", "serve"}, {"fs", "migrate"}, {"fs", "rollback"}, {"fs", "compact"}, {"fs", "recover"},
		{"fs", "enroll", "plan"}, {"fs", "enroll", "apply"},
		{"fs", "namespace", "status"}, {"fs", "namespace", "activate"},
		{"fs", "namespace", "deactivate"}, {"fs", "namespace", "recover"},
		{"fs", "service", "install"}, {"fs", "service", "start"}, {"fs", "service", "stop"},
		{"fs", "service", "status"}, {"fs", "service", "update-binary"}, {"fs", "service", "update-preflight"},
	} {
		if _, _, err := root.Find(commandPath); err != nil {
			t.Fatalf("command %v should be exposed: %v", commandPath, err)
		}
	}
}

func TestDoctorFoldStoreVerifiesPackOnlyManifests(t *testing.T) {
	storeDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := bytes.Repeat([]byte("{\"pack_only\":true}\n"), 256)
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fold.Fold(context.Background(), fold.Session{ID: "session", RolloutPath: source, Archived: true}, fold.FoldOptions{StoreDir: storeDir, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Build(context.Background(), storeDir, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.RetireLoose(context.Background(), storeDir, pack.RetireLooseOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	report, err := doctorFoldStore(context.Background(), storeDir)
	if err != nil || report.IssueCount != 0 || report.VerifiedManifestCount != 1 {
		t.Fatalf("pack-only fold doctor: report=%#v err=%v", report, err)
	}
}

func TestDoctorFoldStoreDoesNotMaskBrokenCurrentPackWithLooseObjects(t *testing.T) {
	storeDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(source, []byte("{\"pack\":\"must remain authoritative\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fold.Fold(context.Background(), fold.Session{ID: "session", RolloutPath: source, Archived: true}, fold.FoldOptions{StoreDir: storeDir, Apply: true}); err != nil {
		t.Fatal(err)
	}
	result, err := pack.Build(context.Background(), storeDir, pack.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	packFiles, err := filepath.Glob(filepath.Join(storeDir, "packs", result.Generation, "*.pack"))
	if err != nil || len(packFiles) == 0 {
		t.Fatalf("locate current pack: files=%v err=%v", packFiles, err)
	}
	if err := os.Remove(packFiles[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := doctorFoldStore(context.Background(), storeDir); err == nil || !strings.Contains(err.Error(), "open current pack") {
		t.Fatalf("broken current pack error = %v", err)
	}
}

func TestRetainCanonicalSnapshotBudgetRejectsBeforeCreatingSnapshot(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "source.jsonl")
	if err := os.WriteFile(sourcePath, []byte("budgeted snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := hashPath(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	checker := &cliRejectingChecker{}
	if _, err := retainCanonicalSnapshot(context.Background(), store, "session", source, checker); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("retainCanonicalSnapshot error = %v, want storage budget rejection", err)
	}
	if checker.Calls != 1 || checker.Projection.Operation != "retain-migration-snapshot" || checker.Projection.AdditionalPersistentBytes != source.Bytes {
		t.Fatalf("unexpected snapshot budget projection: %#v", checker)
	}
	if _, err := os.Stat(filepath.Join(store, "fs", "snapshots")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory exists after preflight rejection: %v", err)
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

func TestFSServiceInstallRendersNativeFSKitDaemonAndSupervisor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native FSKit services are macOS-only")
	}
	home, storeDir, _ := fsFixture(t, true)
	definition := filepath.Join(home, "LaunchAgents", "com.codexfold.fs.plist")
	fskitApp := filepath.Join(home, "Applications", "CodexFoldFSKit.app")
	installedBinary := filepath.Join(home, "bin", "codexfold")
	candidateBinary := filepath.Join(home, "build", "codexfold")
	if err := os.MkdirAll(filepath.Dir(installedBinary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(candidateBinary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installedBinary, []byte("installed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidateBinary, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "service", "install", "--frontend", "native-fskit",
		"--codex-home", home, "--store", storeDir, "--definition", definition,
		"--fskit-app", fskitApp, "--binary", installedBinary, "--binary-source", candidateBinary, "--json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("native FSKit service dry-run: %v", err)
	}
	var result FSServiceInstallResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	wantSupervisor := filepath.Join(filepath.Dir(definition), "com.codexfold.fs.supervisor.plist")
	if !result.DryRun || result.Path != definition || result.SupervisorPath != wantSupervisor || result.SupervisorBytes == 0 {
		t.Fatalf("native FSKit install result = %#v", result)
	}
	for _, path := range []string{definition, wantSupervisor} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry-run wrote %s: %v", path, err)
		}
	}
	launcher := filepath.Join(fskitApp, "Contents", "MacOS", "CodexFoldFSKit")
	if result.FSKitAppPath != fskitApp || result.FSKitLauncherPath != launcher || result.FSKitResourcePath == "" {
		t.Fatalf("native FSKit resolved paths = %#v", result)
	}
	if result.BinarySourcePath != candidateBinary || result.BinaryCurrentSHA256 == "" || result.BinaryCandidateSHA256 == "" || !result.BinaryChanged {
		t.Fatalf("native FSKit binary transaction = %#v", result)
	}
	if content, err := os.ReadFile(installedBinary); err != nil || string(content) != "installed" {
		t.Fatalf("dry-run changed installed binary: content=%q err=%v", content, err)
	}
}

func TestFSServiceInstallDryRunAcceptsMissingBinaryTarget(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native FSKit services are macOS-only")
	}
	home, storeDir, _ := fsFixture(t, true)
	definition := filepath.Join(home, "LaunchAgents", "com.codexfold.fs.plist")
	installedBinary := filepath.Join(home, "new", "codexfold")
	candidateBinary := filepath.Join(home, "build", "codexfold")
	if err := os.MkdirAll(filepath.Dir(candidateBinary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidateBinary, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "service", "install", "--frontend", "native-fskit",
		"--codex-home", home, "--store", storeDir, "--definition", definition,
		"--binary", installedBinary, "--binary-source", candidateBinary, "--json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("native FSKit first-install dry-run: %v", err)
	}
	var result FSServiceInstallResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.BinaryCurrentSHA256 != "" || result.BinaryCandidateSHA256 == "" || !result.BinaryChanged {
		t.Fatalf("first-install binary transaction = %#v", result)
	}
	if _, err := os.Stat(installedBinary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created installed target: %v", err)
	}
}

func TestFSServiceInstallRejectsNativeFSKitResourceWithOverlongSocketPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native FSKit services are macOS-only")
	}
	home, storeDir, _ := fsFixture(t, true)
	resource := filepath.Join(string(filepath.Separator), strings.Repeat("a", 110))
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "service", "install", "--frontend", "native-fskit",
		"--codex-home", home, "--store", storeDir, "--fskit-resource", resource,
	})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "Unix socket path exceeds the macOS limit") {
		t.Fatalf("overlong native FSKit resource error = %v", err)
	}
}

func TestFSServiceInstallCustomLabelUsesCustomDefinitionPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("custom LaunchAgent labels are macOS-only")
	}
	home, storeDir, _ := fsFixture(t, true)
	label := "com.codexfold.native-service-e2e"
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	defaultDefinition := filepath.Join(userHome, "Library", "LaunchAgents", "com.codexfold.fs.plist")
	customDefinition := filepath.Join(userHome, "Library", "LaunchAgents", label+".plist")
	defaultBefore, defaultErr := os.ReadFile(defaultDefinition)
	defaultExists := defaultErr == nil
	if defaultErr != nil && !errors.Is(defaultErr, os.ErrNotExist) {
		t.Fatal(defaultErr)
	}
	customBefore, customErr := os.ReadFile(customDefinition)
	customExists := customErr == nil
	if customErr != nil && !errors.Is(customErr, os.ErrNotExist) {
		t.Fatal(customErr)
	}
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "service", "install", "--frontend", "native-fskit",
		"--label", label, "--codex-home", home, "--store", storeDir, "--json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("custom native FSKit service dry-run: %v", err)
	}
	var result FSServiceInstallResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	wantSupervisor := filepath.Join(userHome, "Library", "LaunchAgents", label+".supervisor.plist")
	if result.Path != customDefinition || result.SupervisorPath != wantSupervisor || !result.DryRun {
		t.Fatalf("custom service install result = %#v", result)
	}
	defaultAfter, defaultErr := os.ReadFile(defaultDefinition)
	if (defaultErr == nil) != defaultExists || (defaultExists && !bytes.Equal(defaultBefore, defaultAfter)) {
		t.Fatalf("custom dry-run changed default definition %s", defaultDefinition)
	}
	customAfter, customErr := os.ReadFile(customDefinition)
	if (customErr == nil) != customExists || (customExists && !bytes.Equal(customBefore, customAfter)) {
		t.Fatalf("custom dry-run changed custom definition %s", customDefinition)
	}
	if _, err := os.Stat(wantSupervisor); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom dry-run wrote supervisor %s: %v", wantSupervisor, err)
	}
}

func TestFSServiceRestartIsExposedAsDryRun(t *testing.T) {
	home := t.TempDir()
	definition := filepath.Join(home, "com.codexfold.fs.plist")
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "service", "restart", "--definition", definition, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var result FSServiceActionResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Action != "restart" || !result.DryRun || result.Path != definition {
		t.Fatalf("restart dry-run = %#v", result)
	}
}

func TestFSUpdatePreflightReportsUnknownClientWithoutChangingManagedRoute(t *testing.T) {
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
	root.SetArgs([]string{"fs", "service", "update-preflight", "--codex-home", home, "--store", storeDir, "--cli", unknownCLI, "--desktop-app", "none", "--apply-quarantine", "--promote", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update preflight: %v", err)
	}
	var result FSUpdatePreflightResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Decision.Allowed || result.Decision.Quarantine || result.Decision.RequiresNativeFallback || result.QuarantinedSessions != 0 || !result.Compatibility.Evaluation.Quarantine {
		t.Fatalf("unexpected diagnostic result: %#v err=%v output=%s", result, err, output.String())
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || len(sessions) != 1 || sessions[0].RolloutPath != target {
		t.Fatalf("client diagnostic changed managed route: sessions=%#v err=%v", sessions, err)
	}
	state, err = managedState(storeDir, "session")
	if err != nil {
		t.Fatalf("client diagnostic retired managed state: %v", err)
	}
	managed, resolver, err = openManagedSession(context.Background(), storeDir, state)
	if err != nil {
		t.Fatal(err)
	}
	visiblePath := filepath.Join(t.TempDir(), "visible.jsonl")
	_, materializeErr := managed.MaterializeCurrent(context.Background(), visiblePath, true)
	_ = resolver.Close()
	if materializeErr != nil {
		t.Fatal(materializeErr)
	}
	visibleBytes, err := os.ReadFile(visiblePath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), original...), tail...)
	if !bytes.Equal(visibleBytes, want) {
		t.Fatalf("client diagnostic changed visible bytes: got=%q want=%q", visibleBytes, want)
	}
	if nativeBytes, err := os.ReadFile(nativePath); err != nil || !bytes.Equal(nativeBytes, original) {
		t.Fatalf("client diagnostic changed native source: got=%q err=%v", nativeBytes, err)
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
	if err := json.Unmarshal(output.Bytes(), &doctor); err != nil || doctor.IssueCount != 0 || doctor.ManifestCount != 1 || doctor.VerifiedManifestCount != 1 {
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

func TestFSMigrateAcceptsAnActiveSessionForCanonicalRouting(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, false)
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount")})
	if err := root.Execute(); err != nil {
		t.Fatalf("active-session migrate dry-run: %v", err)
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || len(sessions) != 1 || sessions[0].RolloutPath != nativePath {
		t.Fatalf("active migration dry-run changed route: sessions=%#v err=%v", sessions, err)
	}
}

func TestFSMigrateRejectsInvalidNativeRolloutBeforeCreatingManagedState(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "invalid UTF-8", data: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n'}, want: "not valid UTF-8"},
		{name: "invalid JSON", data: []byte("not-json\n"), want: "not valid JSON"},
		{name: "missing final newline", data: []byte("{\"record\":0}"), want: "missing its final newline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, storeDir, nativePath := fsFixture(t, true)
			if err := os.WriteFile(nativePath, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			root := NewRootCommand()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount")})
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("migrate error = %v, want %q", err, test.want)
			}
			sessions, loadErr := codex.LoadSessions(home)
			if loadErr != nil || len(sessions) != 1 || filepath.Clean(sessions[0].RolloutPath) != filepath.Clean(nativePath) {
				t.Fatalf("invalid migration changed route: sessions=%#v err=%v", sessions, loadErr)
			}
			states, stateErr := vfs.DiscoverSessionStates(storeDir)
			if stateErr != nil || len(states) != 0 {
				t.Fatalf("invalid migration created managed state: states=%#v err=%v", states, stateErr)
			}
		})
	}
}

func TestFSEnrollmentPlanRequiresTwoStableObservationsAndCanaryGate(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	allowEnrollmentWriterProbe(t)
	allowEnrollmentNamespaceReadiness(t)
	oldProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = oldProbe })
	mount := filepath.Join(home, "mount")
	nativeRoot := filepath.Join(home, "fold-native")
	args := []string{
		"fs", "enroll", "plan", "--codex-home", home, "--store", storeDir, "--mount", mount,
		"--canonical-namespace", "--native-root", nativeRoot, "--enrollment-canary", "--stable-for", "1ns", "--record-observations", "--json",
	}
	runPlan := func() enroll.Plan {
		root := NewRootCommand()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("enrollment plan: %v", err)
		}
		var plan enroll.Plan
		if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
			t.Fatalf("decode enrollment plan: %v\n%s", err, output.String())
		}
		return plan
	}
	first := runPlan()
	if len(first.Selected) != 0 {
		t.Fatalf("first observation selected enrollment: %#v", first)
	}
	time.Sleep(time.Millisecond)
	second := runPlan()
	if len(second.Selected) != 1 || second.Selected[0].SessionID != "session" {
		t.Fatalf("second stable observation was not selected: %#v", second)
	}
}

func TestEnrollmentPlanRunsFullStorageHealthOnlyForCandidateBatch(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	allowEnrollmentWriterProbe(t)
	allowEnrollmentNamespaceReadiness(t)
	oldMountProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = oldMountProbe })
	oldHealthProbe := enrollmentStorageHealthProbe
	healthCalls := 0
	enrollmentStorageHealthProbe = func(context.Context, string) error {
		healthCalls++
		return errors.New("corrupt store")
	}
	t.Cleanup(func() { enrollmentStorageHealthProbe = oldHealthProbe })
	flags := enrollmentFlags{
		codexHome: home, storeDir: storeDir, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true,
		stableFor: time.Nanosecond, batchSize: 1,
	}

	first, _, err := buildEnrollmentPlan(context.Background(), flags)
	if err != nil {
		t.Fatal(err)
	}
	if healthCalls != 0 || len(first.Selected) != 0 {
		t.Fatalf("candidate-free plan ran full health: calls=%d plan=%#v", healthCalls, first)
	}
	if err := enroll.SaveObservations(enrollmentObservationPath(storeDir), first.Observations); err != nil {
		t.Fatal(err)
	}
	second, _, err := buildEnrollmentPlan(context.Background(), flags)
	if err != nil {
		t.Fatal(err)
	}
	if healthCalls != 1 || len(second.Selected) != 0 {
		t.Fatalf("candidate plan health calls=%d plan=%#v", healthCalls, second)
	}
	foundDoctorReason := false
	for _, decision := range second.Decisions {
		if decision.SessionID != "session" {
			continue
		}
		for _, reason := range decision.Reasons {
			foundDoctorReason = foundDoctorReason || reason == enroll.ReasonDoctorUnhealthy
		}
	}
	if !foundDoctorReason {
		t.Fatalf("unhealthy candidate plan did not report doctor gate: %#v", second)
	}
}

func TestFSEnrollmentApplyRunsFoldPackMigrateAndStopsBeforeRouteOnFailure(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	allowEnrollmentWriterProbe(t)
	allowEnrollmentNamespaceReadiness(t)
	oldProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = oldProbe })
	observationPath := enrollmentObservationPath(storeDir)
	info, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveObservations(observationPath, enroll.Observations{"session": {
		Path: nativePath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), StableSinceUnixNano: time.Now().Add(-time.Hour).UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}
	oldRunner := runEnrollmentCommand
	defer func() { runEnrollmentCommand = oldRunner }()
	var calls [][]string
	runEnrollmentCommand = func(_ context.Context, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		if len(calls) == 2 {
			return errors.New("pack failed")
		}
		return nil
	}
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "enroll", "apply", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount"),
		"--canonical-namespace", "--native-root", filepath.Join(home, "fold-native"), "--enrollment-canary", "--stable-for", "1ns", "--apply",
	})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "pack failed") {
		t.Fatalf("enrollment apply error = %v, want pack failure", err)
	}
	if len(calls) != 2 || len(calls[0]) < 2 || calls[0][0] != "fold" || calls[1][0] != "pack" {
		t.Fatalf("enrollment command sequence = %#v", calls)
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || len(sessions) != 1 || filepath.Clean(sessions[0].RolloutPath) != filepath.Clean(nativePath) {
		t.Fatalf("failed enrollment changed route: sessions=%#v err=%v", sessions, err)
	}
	if data, err := os.ReadFile(nativePath); err != nil || len(data) == 0 {
		t.Fatalf("failed enrollment changed source: bytes=%d err=%v", len(data), err)
	}
}

func TestFSEnrollmentApplyBatchesFoldAndPackBeforeMigration(t *testing.T) {
	home, storeDir, firstPath := fsFixture(t, true)
	secondPath := addEnrollmentFixtureSession(t, home, "second", 0)
	allowEnrollmentWriterProbe(t)
	allowEnrollmentNamespaceReadiness(t)
	oldProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = oldProbe })
	saveEnrollmentObservations(t, storeDir, map[string]string{"session": firstPath, "second": secondPath})

	oldRunner := runEnrollmentCommand
	var calls [][]string
	runEnrollmentCommand = func(_ context.Context, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}
	t.Cleanup(func() { runEnrollmentCommand = oldRunner })
	oldGC := runEnrollmentStorageGC
	runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
		return storage.StorageGCResult{}, nil
	}
	t.Cleanup(func() { runEnrollmentStorageGC = oldGC })

	result, err := runEnrollmentCycle(context.Background(), enrollmentFlags{
		codexHome: home, storeDir: storeDir, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true,
		stableFor: time.Nanosecond, batchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Apply.Selected != 2 || result.Apply.Applied != 2 {
		t.Fatalf("batch enrollment result = %#v", result.Apply)
	}
	if len(calls) < 5 {
		t.Fatalf("batch enrollment commands = %#v", calls)
	}
	if calls[0][0] != "fold" || calls[1][0] != "fold" || calls[2][0] != "pack" || calls[2][1] != "build" || calls[3][0] != "fs" || calls[3][1] != "migrate" || calls[4][0] != "fs" || calls[4][1] != "migrate" {
		t.Fatalf("batch enrollment command order = %#v", calls)
	}
	packBuilds := 0
	for _, call := range calls {
		if len(call) >= 2 && call[0] == "pack" && call[1] == "build" {
			packBuilds++
		}
	}
	if packBuilds != 1 {
		t.Fatalf("batch enrollment pack builds = %d, calls=%#v", packBuilds, calls)
	}
}

func TestFSEnrollmentApplyPreservesPartialMigrationProgress(t *testing.T) {
	home, storeDir, firstPath := fsFixture(t, true)
	secondPath := addEnrollmentFixtureSession(t, home, "second", 0)
	firstBefore, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondBefore, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	allowEnrollmentWriterProbe(t)
	allowEnrollmentNamespaceReadiness(t)
	oldProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = oldProbe })
	saveEnrollmentObservations(t, storeDir, map[string]string{"session": firstPath, "second": secondPath})

	oldRunner := runEnrollmentCommand
	migratedID := ""
	runEnrollmentCommand = func(_ context.Context, args []string) error {
		if len(args) < 3 || args[0] != "fs" || args[1] != "migrate" {
			return nil
		}
		if migratedID == "" {
			migratedID = args[2]
			db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
			if err != nil {
				return err
			}
			_, updateErr := db.Exec(`update threads set rollout_path = ? where id = ?`, filepath.Join(home, "mount", "managed", migratedID+".jsonl"), migratedID)
			closeErr := db.Close()
			return errors.Join(updateErr, closeErr)
		}
		return errors.New("second migration failed")
	}
	t.Cleanup(func() { runEnrollmentCommand = oldRunner })
	oldDiscover := discoverEnrollmentSessionStates
	discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) {
		t.Fatal("maintenance discovery ran after partial migration failure")
		return nil, nil
	}
	t.Cleanup(func() { discoverEnrollmentSessionStates = oldDiscover })
	oldGC := runEnrollmentStorageGC
	runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
		t.Fatal("maintenance GC ran after partial migration failure")
		return storage.StorageGCResult{}, nil
	}
	t.Cleanup(func() { runEnrollmentStorageGC = oldGC })

	result, err := runEnrollmentCycle(context.Background(), enrollmentFlags{
		codexHome: home, storeDir: storeDir, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true,
		stableFor: time.Nanosecond, batchSize: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "second migration failed") {
		t.Fatalf("partial migration error = %v", err)
	}
	if result.Apply.Applied != 1 {
		t.Fatalf("partial migration applied = %d", result.Apply.Applied)
	}
	sessions, loadErr := codex.LoadSessions(home)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	routes := make(map[string]string, len(sessions))
	for _, session := range sessions {
		routes[session.ID] = filepath.Clean(session.RolloutPath)
	}
	if routes[migratedID] == filepath.Clean(firstPath) || routes[migratedID] == filepath.Clean(secondPath) {
		t.Fatalf("successful migration remained native: id=%s routes=%#v", migratedID, routes)
	}
	remainingID := "session"
	remainingPath := firstPath
	if migratedID == "session" {
		remainingID = "second"
		remainingPath = secondPath
	}
	if routes[remainingID] != filepath.Clean(remainingPath) {
		t.Fatalf("failed migration changed native route: id=%s routes=%#v", remainingID, routes)
	}
	for path, want := range map[string][]byte{firstPath: firstBefore, secondPath: secondBefore} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("partial migration changed native source %s: bytes=%d err=%v", path, len(got), readErr)
		}
	}
}

func TestFSEnrollmentApplyRejectsInvalidNativeRolloutBeforeCommands(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	allowEnrollmentWriterProbe(t)
	allowFixtureMount(t)
	invalid := []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n'}
	if err := os.WriteFile(nativePath, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveObservations(enrollmentObservationPath(storeDir), enroll.Observations{"session": {
		Path: nativePath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), StableSinceUnixNano: time.Now().Add(-time.Hour).UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}
	oldRunner := runEnrollmentCommand
	defer func() { runEnrollmentCommand = oldRunner }()
	runEnrollmentCommand = func(context.Context, []string) error {
		t.Fatal("invalid rollout reached an enrollment mutation command")
		return nil
	}
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "enroll", "apply", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount"),
		"--canonical-namespace", "--native-root", filepath.Join(home, "fold-native"), "--enrollment-canary", "--stable-for", "1ns", "--apply",
	})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("enrollment error = %v", err)
	}
	states, err := vfs.DiscoverSessionStates(storeDir)
	if err != nil || len(states) != 0 {
		t.Fatalf("invalid enrollment created managed state: states=%#v err=%v", states, err)
	}
}

func TestFSEnrollmentApplyRechecksWriterImmediatelyBeforeCommands(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, false)
	allowFixtureMount(t)
	info, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveObservations(enrollmentObservationPath(storeDir), enroll.Observations{"session": {
		Path: nativePath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), StableSinceUnixNano: time.Now().Add(-time.Hour).UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}
	previousProbe := enrollmentWriterProbe
	probeCalls := 0
	enrollmentWriterProbe = func(context.Context, []codex.Session) (map[string]bool, error) {
		probeCalls++
		if probeCalls >= 2 {
			return map[string]bool{"session": true}, nil
		}
		return map[string]bool{}, nil
	}
	t.Cleanup(func() { enrollmentWriterProbe = previousProbe })
	previousRunner := runEnrollmentCommand
	runEnrollmentCommand = func(context.Context, []string) error {
		t.Fatal("writer-active session reached an enrollment mutation command")
		return nil
	}
	t.Cleanup(func() { runEnrollmentCommand = previousRunner })
	result, err := runEnrollmentCycle(context.Background(), enrollmentFlags{
		codexHome: home, storeDir: storeDir, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true,
		stableFor: time.Nanosecond, batchSize: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "active native writer after enrollment planning") {
		t.Fatalf("writer recheck error = %v", err)
	}
	if probeCalls != 2 || len(result.Plan.Selected) != 1 || result.Apply.Applied != 0 {
		t.Fatalf("writer recheck result=%#v probe_calls=%d", result, probeCalls)
	}
	states, stateErr := vfs.DiscoverSessionStates(storeDir)
	if stateErr != nil || len(states) != 0 {
		t.Fatalf("writer recheck created managed state: states=%#v err=%v", states, stateErr)
	}
}

func TestEnrollmentMaintenanceRetiresNativeLooseAndOldGenerations(t *testing.T) {
	previousDiscover := discoverEnrollmentSessionStates
	discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) {
		return []vfs.SessionState{
			{SessionID: "retire", NativeSnapshot: vfs.NativeFile{Path: "/snapshot/retire.jsonl"}},
			{SessionID: "busy", NativeSnapshot: vfs.NativeFile{Path: "/snapshot/busy.jsonl"}},
			{SessionID: "done"},
		}, nil
	}
	t.Cleanup(func() { discoverEnrollmentSessionStates = previousDiscover })
	previousRunner := runEnrollmentCommand
	var calls [][]string
	runEnrollmentCommand = func(_ context.Context, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 3 && args[0] == "fs" && args[1] == "retire-native" && args[2] == "busy" {
			return errors.New("cannot retire native snapshot while the session has an active writer")
		}
		return nil
	}
	t.Cleanup(func() { runEnrollmentCommand = previousRunner })
	previousGC := runEnrollmentStorageGC
	runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
		return storage.StorageGCResult{RemovedCount: 3, ActualReclaimedBytes: 4096}, nil
	}
	t.Cleanup(func() { runEnrollmentStorageGC = previousGC })
	result, err := runEnrollmentMaintenance(context.Background(), "/codex", "/store", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.NativeCandidates != 2 || result.NativeRetired != 1 || result.NativeDeferred != 1 || !result.LooseRetirementRan || result.StorageGC.RemovedCount != 3 {
		t.Fatalf("maintenance result = %#v", result)
	}
	if len(result.DeferredSessionIDs) != 1 || result.DeferredSessionIDs[0] != "busy" {
		t.Fatalf("maintenance deferred sessions = %#v", result.DeferredSessionIDs)
	}
	if len(calls) != 3 || calls[0][0] != "fs" || calls[0][1] != "retire-native" || calls[1][2] != "busy" || calls[2][0] != "pack" || calls[2][1] != "retire-loose" {
		t.Fatalf("maintenance command sequence = %#v", calls)
	}
}

func TestEnrollmentCycleSkipsMaintenanceAfterApplyFailure(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	allowEnrollmentWriterProbe(t)
	allowEnrollmentNamespaceReadiness(t)
	oldMountProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = oldMountProbe })
	info, err := os.Stat(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := enroll.SaveObservations(enrollmentObservationPath(storeDir), enroll.Observations{"session": {
		Path: nativePath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), StableSinceUnixNano: time.Now().Add(-time.Hour).UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}
	previousRunner := runEnrollmentCommand
	previousDiscover := discoverEnrollmentSessionStates
	previousGC := runEnrollmentStorageGC
	runEnrollmentCommand = func(_ context.Context, args []string) error {
		if len(args) >= 2 && args[0] == "pack" && args[1] == "build" {
			return errors.New("pack failed")
		}
		return nil
	}
	discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) {
		t.Fatal("maintenance discovery ran after apply failure")
		return nil, nil
	}
	runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
		t.Fatal("maintenance GC ran after apply failure")
		return storage.StorageGCResult{}, nil
	}
	t.Cleanup(func() {
		runEnrollmentCommand = previousRunner
		discoverEnrollmentSessionStates = previousDiscover
		runEnrollmentStorageGC = previousGC
	})
	result, err := runEnrollmentCycle(context.Background(), enrollmentFlags{
		codexHome: home, storeDir: storeDir, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true, canary: true,
		stableFor: time.Nanosecond, batchSize: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "pack failed") {
		t.Fatalf("apply failure = %v", err)
	}
	if result.Maintenance.NativeCandidates != 0 || result.Maintenance.LooseRetirementRan {
		t.Fatalf("maintenance ran after apply failure: %#v", result.Maintenance)
	}
}

func TestEnrollmentMaintenanceSurfacesVerificationFailure(t *testing.T) {
	previousDiscover := discoverEnrollmentSessionStates
	discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) {
		return []vfs.SessionState{{SessionID: "broken", NativeSnapshot: vfs.NativeFile{Path: "/snapshot/broken.jsonl"}}}, nil
	}
	t.Cleanup(func() { discoverEnrollmentSessionStates = previousDiscover })
	previousRunner := runEnrollmentCommand
	runEnrollmentCommand = func(_ context.Context, args []string) error {
		if args[0] == "fs" {
			return errors.New("pack-only recovery proof failed")
		}
		return nil
	}
	t.Cleanup(func() { runEnrollmentCommand = previousRunner })
	previousGC := runEnrollmentStorageGC
	runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
		return storage.StorageGCResult{}, nil
	}
	t.Cleanup(func() { runEnrollmentStorageGC = previousGC })
	result, err := runEnrollmentMaintenance(context.Background(), "/codex", "/store", 0)
	if err == nil || !strings.Contains(err.Error(), "pack-only recovery proof failed") {
		t.Fatalf("maintenance verification error = %v", err)
	}
	if result.NativeRetired != 0 || result.NativeDeferred != 1 || !result.LooseRetirementRan {
		t.Fatalf("maintenance verification result = %#v", result)
	}
}

func TestPeriodicEnrollmentLoopRunsSerialCyclesAndStopsWithContext(t *testing.T) {
	oldRunner := runServiceEnrollmentCycle
	defer func() { runServiceEnrollmentCycle = oldRunner }()

	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	flagErrors := make(chan error, 1)
	runServiceEnrollmentCycle = func(ctx context.Context, flags enrollmentFlags) (FSEnrollmentApplyResult, error) {
		if flags.batchSize != 3 || flags.stableFor != 2*time.Hour || !flags.canonicalNamespace {
			select {
			case flagErrors <- fmt.Errorf("unexpected enrollment flags: %#v", flags):
			default:
			}
		}
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return FSEnrollmentApplyResult{}, ctx.Err()
		case <-release:
			return FSEnrollmentApplyResult{}, nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runPeriodicEnrollment(ctx, enrollmentFlags{batchSize: 3, stableFor: 2 * time.Hour, canonicalNamespace: true}, time.Millisecond, nil)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("periodic enrollment did not run")
	}
	select {
	case <-started:
		t.Fatal("periodic enrollment overlapped a running cycle")
	case <-time.After(10 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("periodic enrollment did not schedule the next cycle")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("periodic enrollment did not stop with its context")
	}
	select {
	case err := <-flagErrors:
		t.Fatal(err)
	default:
	}
}

func TestEnrollmentChildEnvironmentDropsLauncherParentOnly(t *testing.T) {
	got := enrollmentChildEnvironment([]string{"HOME=/tmp/home", "CODEXFOLD_LAUNCHER_PARENT_PID=42", "PATH=/usr/bin"})
	if len(got) != 2 || got[0] != "HOME=/tmp/home" || got[1] != "PATH=/usr/bin" {
		t.Fatalf("enrollment child environment = %#v", got)
	}
}

func TestEnrollmentCycleInPreviewRecordsObservationsWithoutMutating(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	allowEnrollmentWriterProbe(t)
	allowEnrollmentNamespaceReadiness(t)
	oldProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = oldProbe })
	oldRunner := runEnrollmentCommand
	defer func() { runEnrollmentCommand = oldRunner }()
	runEnrollmentCommand = func(context.Context, []string) error {
		t.Fatal("preview enrollment cycle attempted a mutation")
		return nil
	}

	result, err := runEnrollmentCycle(context.Background(), enrollmentFlags{
		codexHome: home, storeDir: storeDir, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true,
		stableFor: time.Nanosecond, batchSize: 1,
	})
	if err != nil {
		t.Fatalf("runEnrollmentCycle: %v", err)
	}
	if result.Apply.Applied != 0 || len(result.Plan.Selected) != 0 {
		t.Fatalf("preview cycle selected or applied sessions: %#v", result)
	}
	observations, err := enroll.LoadObservations(enrollmentObservationPath(storeDir))
	if err != nil || len(observations) != 1 {
		t.Fatalf("preview cycle did not persist observations: observations=%#v err=%v", observations, err)
	}
}

func allowEnrollmentWriterProbe(t *testing.T) {
	t.Helper()
	oldProbe := enrollmentWriterProbe
	enrollmentWriterProbe = func(context.Context, []codex.Session) (map[string]bool, error) {
		return map[string]bool{}, nil
	}
	t.Cleanup(func() { enrollmentWriterProbe = oldProbe })
}

func TestEnrollmentStorageHealthAllowsBootstrapButRejectsBrokenCommittedPack(t *testing.T) {
	storeDir := t.TempDir()
	if err := requireEnrollmentStorageHealth(context.Background(), storeDir); err != nil {
		t.Fatalf("empty enrollment store should be a valid bootstrap state: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(storeDir, "packs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "packs", "CURRENT"), []byte("missing-generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireEnrollmentStorageHealth(context.Background(), storeDir); err == nil {
		t.Fatal("a broken committed pack generation passed enrollment health")
	}
}

func TestCurrentPackForStatesAllowsOnlyPristineBootstrapStore(t *testing.T) {
	storeDir := t.TempDir()
	current, err := currentPackForStates(storeDir, nil)
	if err != nil || current != "" {
		t.Fatalf("pristine current pack = %q, %v", current, err)
	}
	if _, err := currentPackForStates(storeDir, []vfs.SessionState{{SessionID: "session"}}); err == nil {
		t.Fatal("managed state without a committed pack should fail")
	}
	if err := os.MkdirAll(filepath.Join(storeDir, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "objects", "partial.zst"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := currentPackForStates(storeDir, nil); err == nil {
		t.Fatal("partial store without a committed pack should fail")
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

func TestFSMigrateApplyRejectsActiveNativeWriterBeforeMutation(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	previousProbe := filesystemMigrationWriterProbe
	filesystemMigrationWriterProbe = func(context.Context, codex.Session, ...string) (bool, error) { return true, nil }
	t.Cleanup(func() { filesystemMigrationWriterProbe = previousProbe })
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount"), "--apply"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "active native writer") {
		t.Fatalf("writer-active migration error = %v", err)
	}
	sessions, loadErr := codex.LoadSessions(home)
	if loadErr != nil || len(sessions) != 1 || sessions[0].RolloutPath != nativePath {
		t.Fatalf("writer-active migration changed route: sessions=%#v err=%v", sessions, loadErr)
	}
	states, stateErr := vfs.DiscoverSessionStates(storeDir)
	if stateErr != nil || len(states) != 0 {
		t.Fatalf("writer-active migration created managed state: states=%#v err=%v", states, stateErr)
	}
}

func TestFSMigrateApplyRejectsSourceChangeDuringWriterProbe(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	previousProbe := filesystemMigrationWriterProbe
	filesystemMigrationWriterProbe = func(_ context.Context, session codex.Session, _ ...string) (bool, error) {
		file, err := os.OpenFile(session.RolloutPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return false, err
		}
		_, writeErr := file.WriteString("{\"changed_during_probe\":true}\n")
		return false, errors.Join(writeErr, file.Close())
	}
	t.Cleanup(func() { filesystemMigrationWriterProbe = previousProbe })
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fs", "migrate", "session", "--codex-home", home, "--store", storeDir, "--mount", filepath.Join(home, "mount"), "--apply"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "changed during writer probe") {
		t.Fatalf("source-change migration error = %v", err)
	}
	sessions, loadErr := codex.LoadSessions(home)
	if loadErr != nil || len(sessions) != 1 || sessions[0].RolloutPath != nativePath {
		t.Fatalf("source-change migration changed route: sessions=%#v err=%v", sessions, loadErr)
	}
	states, stateErr := vfs.DiscoverSessionStates(storeDir)
	if stateErr != nil || len(states) != 0 {
		t.Fatalf("source-change migration created managed state: states=%#v err=%v", states, stateErr)
	}
}

func TestFSMigrateApplyRetiresManagedStateWhenMountedTargetNeverAppears(t *testing.T) {
	allowFixtureMount(t)
	home, storeDir, nativePath := fsFixture(t, true)
	cliPath := approvedCLIContract(t, storeDir, "1.2.3")
	mount := filepath.Join(home, "mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "migrate", "session", "--codex-home", home, "--store", storeDir,
		"--mount", mount, "--mount-wait", "50ms", "--cli", cliPath,
		"--desktop-app", "none", "--apply",
	})
	if err := root.Execute(); err == nil {
		t.Fatal("fs migrate --apply should fail when the mounted target never appears")
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil || sessions[0].RolloutPath != nativePath {
		t.Fatalf("failed apply changed route: sessions=%#v err=%v", sessions, err)
	}
	got, err := os.ReadFile(nativePath)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("failed apply changed source: got=%q err=%v", got, err)
	}
	states, err := vfs.DiscoverSessionStates(storeDir)
	if err != nil || len(states) != 0 {
		t.Fatalf("failed apply left managed state: states=%#v err=%v", states, err)
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
	if _, err := fold.Fold(context.Background(), fold.Session{ID: "session", RolloutPath: route, Archived: true}, fold.FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 8}); err != nil {
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

func TestRollbackCanonicalMigrationRestoresNativeBeforeRetiringManagedState(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	stateDirectory := filepath.Join(store, "fs", "sessions", "session")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, "native", "archived_sessions", "rollout.jsonl")
	retainedPath := filepath.Join(store, "fs", "snapshots", "session", "native.jsonl")
	content := []byte("{\"restored\":true}\n")
	if err := os.MkdirAll(filepath.Dir(retainedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retainedPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("force migration rollback")
	if err := rollbackCanonicalMigration(store, "session", sourcePath, retainedPath, cause); !errors.Is(err, cause) {
		t.Fatalf("rollback error = %v, want original cause", err)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("restored native source = %q err=%v", got, err)
	}
	if _, err := os.Stat(stateDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed state was not retired after native restoration: %v", err)
	}
	retired, err := filepath.Glob(filepath.Join(store, "fs", "retired", "session-*"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("retired states = %v err=%v", retired, err)
	}
}

func TestRollbackCanonicalMigrationKeepsManagedStateWhenNativeRestoreConflicts(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	stateDirectory := filepath.Join(store, "fs", "sessions", "session")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, "native", "archived_sessions", "rollout.jsonl")
	retainedPath := filepath.Join(store, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(retainedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("{\"current\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retainedPath, []byte("{\"retained\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rollbackCanonicalMigration(store, "session", sourcePath, retainedPath, errors.New("force migration rollback")); err == nil {
		t.Fatal("conflicting native restoration unexpectedly succeeded")
	}
	if _, err := os.Stat(stateDirectory); err != nil {
		t.Fatalf("managed state was retired after native restore conflict: %v", err)
	}
	if _, err := os.Stat(retainedPath); err != nil {
		t.Fatalf("retained snapshot was removed after native restore conflict: %v", err)
	}
	retired, err := filepath.Glob(filepath.Join(store, "fs", "retired", "session-*"))
	if err != nil || len(retired) != 0 {
		t.Fatalf("retired states after conflict = %v err=%v", retired, err)
	}
}

func TestFSRecoverRetiresInterruptedCanonicalMigrationWithUnchangedSource(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	_ = fixture.resolver.Close()

	executeFS(t, []string{"fs", "recover", "session", "--apply", "--codex-home", fixture.home, "--store", fixture.store})
	if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", "session")); !os.IsNotExist(err) {
		t.Fatalf("interrupted migration state remained: %v", err)
	}
	got, err := os.ReadFile(fixture.nativePath)
	if err != nil || !bytes.Equal(got, fixture.source) {
		t.Fatalf("recovery changed canonical source: got=%q err=%v", got, err)
	}
}

func TestFSRecoverLeavesLiveCanonicalMigrationManaged(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	defer fixture.resolver.Close()
	writer, err := fixture.managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	state := fixture.managed.State()
	recovered, err := recoverInterruptedCanonicalMigration(fixture.home, fixture.store, fixture.nativeRoot, state)
	if err != nil || recovered {
		t.Fatalf("live migration recovery = %t, %v", recovered, err)
	}
	if _, err := managedState(fixture.store, "session"); err != nil {
		t.Fatalf("live migration state was retired: %v", err)
	}
	if got, err := os.ReadFile(fixture.nativePath); err != nil || !bytes.Equal(got, fixture.source) {
		t.Fatalf("live migration source changed: got=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(state.NativeSnapshot.Path); err != nil || !bytes.Equal(got, fixture.source) {
		t.Fatalf("live migration snapshot changed: got=%q err=%v", got, err)
	}
}

func TestFSRecoverDoesNotMisclassifyAProvenRetiredNativeSnapshot(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	defer fixture.resolver.Close()
	original := fixture.managed.State()
	visible, err := fixture.managed.MaterializeCurrent(context.Background(), filepath.Join(fixture.home, "retirement-proof.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := fixture.managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.managed.RetireNativeSnapshot(original.NativeSnapshot, visible); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	stale := fixture.managed.State()
	stale.Generation += 3
	stale.NativeSnapshot = original.NativeSnapshot
	recovered, err := recoverInterruptedCanonicalMigration(fixture.home, fixture.store, fixture.nativeRoot, stale)
	if err != nil || recovered {
		t.Fatalf("proven retirement migration recovery = %t, %v", recovered, err)
	}
	if _, err := managedState(fixture.store, "session"); err != nil {
		t.Fatalf("proven retired snapshot caused managed state retirement: %v", err)
	}
}

func TestFSRecoverLeavesPendingCanonicalRollbackManaged(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	defer fixture.resolver.Close()
	tail := []byte("{\"pending_rollback\":true}\n")
	writer, err := fixture.managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(context.Background(), tail); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := fixture.managed.MaterializeCurrent(context.Background(), fixture.nativePath, true)
	if err != nil {
		t.Fatal(err)
	}
	state := fixture.managed.State()
	if _, err := createRetirementRequest(fixture.store, "session", state.Generation, "/archived_sessions/rollout-session.jsonl", target); err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverInterruptedCanonicalMigration(fixture.home, fixture.store, fixture.nativeRoot, state)
	if err != nil || recovered {
		t.Fatalf("pending rollback recovery = %t, %v", recovered, err)
	}
	if _, err := managedState(fixture.store, "session"); err != nil {
		t.Fatalf("pending rollback state was retired: %v", err)
	}
	if err := clearRetirementControl(filepath.Join(fixture.store, "fs", "sessions", "session")); err != nil {
		t.Fatal(err)
	}
	recovered, err = recoverInterruptedCanonicalMigration(fixture.home, fixture.store, fixture.nativeRoot, state)
	if err != nil || recovered {
		t.Fatalf("pre-request rollback recovery = %t, %v", recovered, err)
	}
	want := append(append([]byte(nil), fixture.source...), tail...)
	if got, err := os.ReadFile(fixture.nativePath); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("pending rollback target changed: got=%q err=%v", got, err)
	}
}

func TestCreateRetirementRequestResumesOnlyExactPendingRequest(t *testing.T) {
	storeDir := t.TempDir()
	directory := filepath.Join(storeDir, "fs", "sessions", "session")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := vfs.NativeFile{Path: filepath.Join(t.TempDir(), "current.jsonl"), Bytes: 42, SHA256: strings.Repeat("a", 64)}
	first, err := createRetirementRequest(storeDir, "session", 3, "/archived_sessions/rollout.jsonl", target)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := createRetirementRequest(storeDir, "session", 3, "/archived_sessions/rollout.jsonl", target)
	if err != nil {
		t.Fatalf("resume exact retirement request: %v", err)
	}
	if resumed != first {
		t.Fatalf("resumed request changed token or metadata: first=%#v resumed=%#v", first, resumed)
	}
	target.SHA256 = strings.Repeat("b", 64)
	if _, err := createRetirementRequest(storeDir, "session", 3, "/archived_sessions/rollout.jsonl", target); err == nil {
		t.Fatal("mismatched pending retirement request should fail closed")
	}
}

type interruptedCanonicalFixture struct {
	home       string
	store      string
	nativeRoot string
	nativePath string
	source     []byte
	managed    *vfs.Session
	resolver   *pack.Resolver
}

func interruptedCanonicalMigrationFixture(t *testing.T) interruptedCanonicalFixture {
	t.Helper()
	home := t.TempDir()
	storeDir := filepath.Join(home, "fold-store")
	route := filepath.Join(home, "archived_sessions", "rollout-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(route), 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte("{\"type\":\"session_meta\"}\n{\"interrupted_migration\":true}\n")
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
	if _, err := fold.Fold(context.Background(), fold.Session{ID: "session", RolloutPath: route, Archived: true}, fold.FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 8}); err != nil {
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
	native, err := hashPath(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := retainCanonicalSnapshot(context.Background(), storeDir, "session", native, nil)
	if err != nil {
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
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest,
		Reader: resolver, NativeSnapshot: retained,
	})
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	return interruptedCanonicalFixture{
		home: home, store: storeDir, nativeRoot: nativeRoot, nativePath: nativePath,
		source: source, managed: managed, resolver: resolver,
	}
}

func TestFSMigrateCanonicalReservesWriterDuringCutover(t *testing.T) {
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
	if _, err := fold.Fold(context.Background(), fold.Session{ID: "session", RolloutPath: route, Archived: true}, fold.FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 8}); err != nil {
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
	writerAttempt := make(chan error, 1)
	releaseWriter := make(chan struct{})
	go func() {
		statePath := filepath.Join(storeDir, "fs", "sessions", "session", "state.json")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			state, stateErr := vfs.LoadSessionState(statePath)
			if stateErr == nil {
				if ackErr := writeMountAcknowledgement(storeDir, "session", state.Generation, "/archived_sessions/"+filepath.Base(route)); ackErr != nil {
					writerAttempt <- ackErr
					return
				}
				managed, resolver, openErr := openManagedSession(context.Background(), storeDir, state)
				if openErr != nil {
					writerAttempt <- openErr
					return
				}
				writer, writerErr := managed.OpenWriter()
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err == nil {
					err = os.WriteFile(target, source, 0o600)
				}
				writerAttempt <- writerErr
				if writer != nil {
					<-releaseWriter
					_ = writer.Close()
				}
				_ = resolver.Close()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		writerAttempt <- errors.New("managed state was not created")
	}()

	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "migrate", "session", "--apply", "--canonical-namespace",
		"--codex-home", home, "--store", storeDir, "--mount", mount, "--native-root", nativeRoot,
		"--cli", cliPath, "--desktop-app", "none", "--mount-wait", "500ms",
	})
	migrateErr := root.Execute()
	writerErr := <-writerAttempt
	close(releaseWriter)
	if migrateErr != nil {
		t.Fatalf("canonical migration failed: %v", migrateErr)
	}
	if !errors.Is(writerErr, vfs.ErrWriterBusy) {
		t.Fatalf("concurrent writer error = %v, want %v", writerErr, vfs.ErrWriterBusy)
	}
	if _, err := managedState(storeDir, "session"); err != nil {
		t.Fatalf("canonical migration did not retain managed state: %v", err)
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
	copyDone := emulateCanonicalRetirement(storeDir, "session", mountedTarget, targetNativePath)
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

func TestFSRollbackCanonicalRetirementUsesRecoveredGeneration(t *testing.T) {
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
		_ = resolver.Close()
		t.Fatal(err)
	}
	recoveryStop := errors.New("stop after COW file publish")
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest,
		Reader: resolver, NativeSnapshot: native,
		BeforeCOWPhase: func(phase string) error {
			if phase == "after-file-publish" {
				return recoveryStop
			}
			return nil
		},
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
	if _, err := writer.WriteAt(context.Background(), []byte("X"), 0); !errors.Is(err, recoveryStop) {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatalf("WriteAt error = %v, want %v", err, recoveryStop)
	}
	if err := writer.Close(); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}
	staleState, err := managedState(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	if staleState.Generation != 1 {
		t.Fatalf("pre-recovery generation = %d, want 1", staleState.Generation)
	}

	mount := filepath.Join(home, "fold-fs")
	mountedTarget := filepath.Join(mount, "sessions", "2026", "07", "12", filename)
	if err := os.MkdirAll(filepath.Dir(mountedTarget), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mountedTarget, original, 0o600); err != nil {
		t.Fatal(err)
	}
	retirementDone := emulateCanonicalRetirementGeneration(t, storeDir, "session", mountedTarget, targetNativePath, 2)

	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "rollback", "session", "--apply", "--canonical-namespace",
		"--codex-home", home, "--store", storeDir, "--mount", mount, "--native-root", nativeRoot,
		"--mount-wait", "500ms",
	})
	rollbackErr := root.Execute()
	if err := <-retirementDone; err != nil {
		t.Fatal(err)
	}
	if rollbackErr != nil {
		t.Fatalf("rollback with recovered session state: %v", rollbackErr)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "fs", "sessions", "session")); !os.IsNotExist(err) {
		t.Fatalf("managed state remained after canonical rollback: %v", err)
	}
	if got, err := os.ReadFile(targetNativePath); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("canonical rollback bytes = %q err=%v", got, err)
	}
}

func TestCanonicalRetirementColdLoadRestoresManagedFallbackBeforeNativePreference(t *testing.T) {
	home, storeDir, originalPath := fsFixture(t, true)
	original, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifest(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	retainedPath := filepath.Join(storeDir, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(retainedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retainedPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := pack.Open(storeDir, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := hashPath(retainedPath)
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest,
		Reader: resolver, NativeSnapshot: retained,
	})
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}

	route := "/archived_sessions/rollout-session.jsonl"
	nativeRoot := filepath.Join(home, "fold-native")
	nativeTargetPath := filepath.Join(nativeRoot, "archived_sessions", "rollout-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(nativeTargetPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeTargetPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	nativeTarget, err := hashPath(nativeTargetPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createRetirementRequest(storeDir, "session", managed.State().Generation, route, nativeTarget); err != nil {
		t.Fatal(err)
	}

	filesystem := mountfs.NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	known := make(map[string]uint64)
	knownRoutes := make(map[string]string)
	knownPacks := make(map[string]string)
	currentPack, err := pack.CurrentGeneration(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = filesystem.CloseSessions() })
	opens := 0
	openState := func(state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
		opens++
		current, nextResolver, err := openManagedSession(context.Background(), storeDir, state)
		return current, nextResolver, err
	}
	state, err := managedState(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		handled, err := syncCanonicalRetirement(storeDir, home, nativeRoot, filesystem, state, route, true, known, knownRoutes, knownPacks, currentPack, openState)
		if err != nil || !handled {
			t.Fatalf("sync retirement attempt %d: handled=%t err=%v", attempt, handled, err)
		}
	}
	if opens != 1 || known["session"] != state.Generation || knownRoutes["session"] != route {
		t.Fatalf("cold load state: opens=%d known=%#v routes=%#v", opens, known, knownRoutes)
	}
	acknowledgement, err := os.ReadFile(filepath.Join(storeDir, "fs", "sessions", "session", retirementAcknowledgementFilename))
	if err != nil || !bytes.Contains(acknowledgement, []byte(`"token"`)) {
		t.Fatalf("retirement acknowledgement = %q err=%v", acknowledgement, err)
	}
	if got := readMountedFilesystemFile(t, filesystem, route, len(original)); !bytes.Equal(got, original) {
		t.Fatalf("native-preferred bytes = %q", got)
	}
	if err := os.Remove(nativeTargetPath); err != nil {
		t.Fatal(err)
	}
	if got := readMountedFilesystemFile(t, filesystem, route, len(original)); !bytes.Equal(got, original) {
		t.Fatalf("managed fallback bytes = %q", got)
	}
}

func TestCanonicalRetirementDaemonRestartRejectsStaleNativeAcknowledgement(t *testing.T) {
	home, storeDir, originalPath := fsFixture(t, true)
	original, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifest(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	retainedPath := filepath.Join(storeDir, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(retainedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retainedPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := pack.Open(storeDir, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := hashPath(retainedPath)
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest,
		Reader: resolver, NativeSnapshot: retained,
	})
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	tail := []byte("{\"pending_retirement\":true}\n")
	writer, err := managed.OpenWriter()
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
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
	if err := writer.Close(); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}
	current := append(append([]byte(nil), original...), tail...)

	route := "/archived_sessions/rollout-session.jsonl"
	nativeRoot := filepath.Join(home, "fold-native")
	nativeTargetPath := filepath.Join(nativeRoot, "archived_sessions", "rollout-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(nativeTargetPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeTargetPath, current, 0o600); err != nil {
		t.Fatal(err)
	}
	nativeTarget, err := hashPath(nativeTargetPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := managedState(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	retirement, err := createRetirementRequest(storeDir, "session", state.Generation, route, nativeTarget)
	if err != nil {
		t.Fatal(err)
	}

	opens := 0
	openState := func(state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
		opens++
		current, nextResolver, err := openManagedSession(context.Background(), storeDir, state)
		return current, nextResolver, err
	}

	firstDaemon := mountfs.NewCanonical()
	firstDaemon.SetNativeRoot(nativeRoot)
	t.Cleanup(func() { _ = firstDaemon.CloseSessions() })
	firstKnown := make(map[string]uint64)
	firstRoutes := make(map[string]string)
	firstPacks := make(map[string]string)
	currentPack, err := pack.CurrentGeneration(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	handled, err := syncCanonicalRetirement(storeDir, home, nativeRoot, firstDaemon, state, route, true, firstKnown, firstRoutes, firstPacks, currentPack, openState)
	if err != nil || !handled {
		t.Fatalf("initial retirement sync: handled=%t err=%v", handled, err)
	}
	if got := readMountedFilesystemFile(t, firstDaemon, route, len(current)); !bytes.Equal(got, current) {
		t.Fatalf("native-preferred bytes = %q, want %q", got, current)
	}

	if err := os.Remove(nativeTargetPath); err != nil {
		t.Fatal(err)
	}
	restartedDaemon := mountfs.NewCanonical()
	restartedDaemon.SetNativeRoot(nativeRoot)
	t.Cleanup(func() { _ = restartedDaemon.CloseSessions() })
	restartedKnown := make(map[string]uint64)
	restartedRoutes := make(map[string]string)
	restartedPacks := make(map[string]string)
	handled, err = syncCanonicalRetirement(storeDir, home, nativeRoot, restartedDaemon, state, route, true, restartedKnown, restartedRoutes, restartedPacks, currentPack, openState)
	if err != nil || !handled {
		t.Fatalf("restart retirement sync: handled=%t err=%v", handled, err)
	}
	if opens != 2 {
		t.Fatalf("daemon restarts opened managed state %d times, want 2", opens)
	}
	acknowledgement, err := os.ReadFile(filepath.Join(storeDir, "fs", "sessions", "session", retirementAcknowledgementFilename))
	if err != nil {
		t.Fatal(err)
	}
	var acknowledged retirementControl
	if err := json.Unmarshal(acknowledgement, &acknowledged); err != nil {
		t.Fatal(err)
	}
	if acknowledged.Token != retirement.Token || acknowledged.Error == "" {
		t.Fatalf("stale acknowledgement was not rejected: %#v", acknowledged)
	}
	if got := readMountedFilesystemFile(t, restartedDaemon, route, len(current)); !bytes.Equal(got, current) {
		t.Fatalf("restart managed fallback bytes = %q, want %q", got, current)
	}
}

func TestFSRollbackCanonicalFailureWaitsForManagedRouteRestoration(t *testing.T) {
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
	tail := []byte("{\"rollback_failure\":true}\n")
	if _, err := writer.Append(context.Background(), tail); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = resolver.Close()
	want := append(append([]byte(nil), original...), tail...)

	mount := filepath.Join(home, "fold-fs")
	mountedTarget := filepath.Join(mount, "sessions", "2026", "07", "12", filename)
	if err := os.MkdirAll(filepath.Dir(mountedTarget), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mountedTarget, original, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Join(storeDir, "fs", "sessions", "session")
	retirementRequest := filepath.Join(stateDirectory, "retire.request.json")
	canonicalRoute := "/sessions/2026/07/12/" + filename
	restored := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(stateDirectory); err != nil {
				restored <- fmt.Errorf("managed state moved before retirement request: %w", err)
				return
			}
			request, err := os.ReadFile(retirementRequest)
			if err == nil {
				var rejection retirementControl
				if err := json.Unmarshal(request, &rejection); err != nil {
					restored <- err
					return
				}
				rejection.Error = "native rollback target is unavailable or changed"
				if err := writeRetirementAcknowledgement(storeDir, "session", rejection); err != nil {
					restored <- err
					return
				}
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				restored <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		for time.Now().Before(deadline) {
			if _, err := os.Stat(stateDirectory); err != nil {
				restored <- fmt.Errorf("managed state moved during retirement cancellation: %w", err)
				return
			}
			if _, err := os.Stat(retirementRequest); errors.Is(err, os.ErrNotExist) {
				state, stateErr := managedState(storeDir, "session")
				if stateErr != nil {
					restored <- stateErr
					return
				}
				if state.Generation < 2 {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				if err := os.WriteFile(mountedTarget, want, 0o600); err != nil {
					restored <- err
					return
				}
				restored <- writeMountAcknowledgement(storeDir, "session", state.Generation, canonicalRoute)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		restored <- errors.New("managed route was not restored")
	}()

	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "rollback", "session", "--apply", "--canonical-namespace",
		"--codex-home", home, "--store", storeDir, "--mount", mount, "--native-root", nativeRoot,
		"--mount-wait", "200ms",
	})
	err = root.Execute()
	if err == nil || !strings.Contains(err.Error(), "retirement rejected") {
		t.Fatalf("rollback error = %v, want retirement rejection", err)
	}
	select {
	case restoreErr := <-restored:
		if restoreErr != nil {
			t.Fatal(restoreErr)
		}
	default:
		t.Fatal("rollback returned before the managed route became readable again")
	}
	state, err := managedState(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 2 {
		t.Fatalf("restored generation = %d, want 2", state.Generation)
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
	copyDone := emulateCanonicalRetirement(storeDir, "session", mountedTarget, targetNativePath)
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

func TestFSUpdatePreflightReportsUnknownClientWithoutChangingNativeFallback(t *testing.T) {
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
	root.SetArgs([]string{"fs", "service", "update-preflight", "--codex-home", home, "--store", storeDir, "--cli", unknownCLI, "--desktop-app", "none", "--apply-quarantine", "--promote", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update preflight: %v", err)
	}
	var result FSUpdatePreflightResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode preflight result: %v output=%s", err, output.String())
	}
	if !result.Decision.Allowed || result.Decision.Quarantine || result.Decision.RequiresNativeFallback || result.QuarantinedSessions != 0 || !result.Compatibility.Evaluation.Quarantine {
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
	longLine := append([]byte("{\"tail\":\""), bytes.Repeat([]byte("x"), 5<<10)...)
	longLine = append(longLine, []byte("\"}\n")...)
	if _, err := writer.Append(context.Background(), longLine); err != nil {
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

func TestFSRetireNativeIsDryRunFirstAndKeepsPackOnlyRestartReadable(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	manifest, err := fold.LoadManifest(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	source, err := hashPath(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := retainCanonicalSnapshot(context.Background(), storeDir, "session", source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeCanonicalSnapshotSource(nativePath, retained); err != nil {
		t.Fatal(err)
	}
	resolver, err := pack.Open(storeDir, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest, Reader: resolver, NativeSnapshot: retained,
	}); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	_ = resolver.Close()
	executeFS(t, []string{"fs", "retire-native", "session", "--codex-home", home, "--store", storeDir})
	state, err := managedState(storeDir, "session")
	if err != nil || state.NativeSnapshot != retained {
		t.Fatalf("dry-run changed native snapshot: state=%#v err=%v", state, err)
	}
	if _, err := os.Stat(retained.Path); err != nil {
		t.Fatalf("dry-run removed retained snapshot: %v", err)
	}
	retainedBytes, err := os.ReadFile(retained.Path)
	if err != nil {
		t.Fatal(err)
	}
	executeFS(t, []string{"fs", "retire-native", "session", "--codex-home", home, "--store", storeDir, "--apply"})
	state, err = managedState(storeDir, "session")
	if err != nil || state.NativeSnapshot.Path != "" {
		t.Fatalf("apply did not clear native snapshot: state=%#v err=%v", state, err)
	}
	if _, err := os.Stat(retained.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired snapshot still exists: %v", err)
	}
	proofPath := filepath.Join(storeDir, "fs", "sessions", "session", vfs.NativeRetirementFilename)
	proof, err := vfs.LoadNativeRetirementProof(proofPath)
	if err != nil || proof.Snapshot != retained || proof.Visible.SHA256 != manifest.Source.SHA256 {
		t.Fatalf("native retirement proof = %#v err=%v", proof, err)
	}
	reopened, nextResolver, err := openManagedSession(context.Background(), storeDir, state)
	if err != nil {
		t.Fatal(err)
	}
	current, err := reopened.MaterializeCurrent(context.Background(), filepath.Join(home, "pack-only-current.jsonl"), false)
	_ = nextResolver.Close()
	if err != nil || current.SHA256 != manifest.Source.SHA256 || current.Bytes != manifest.Source.Bytes {
		t.Fatalf("pack-only restart materialization = %#v err=%v", current, err)
	}
	if err := os.MkdirAll(filepath.Dir(retained.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retained.Path, retainedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	executeFS(t, []string{"fs", "retire-native", "session", "--codex-home", home, "--store", storeDir, "--apply"})
	if _, err := os.Stat(retained.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry did not remove a verified interrupted-retirement snapshot: %v", err)
	}
	rollbackPath := filepath.Join(home, "rollback-after-native-retirement.jsonl")
	executeFS(t, []string{"fs", "rollback", "session", "--codex-home", home, "--store", storeDir, "--to", rollbackPath, "--apply"})
	rolledBack, err := hashPath(rollbackPath)
	if err != nil || rolledBack.SHA256 != manifest.Source.SHA256 || rolledBack.Bytes != manifest.Source.Bytes {
		t.Fatalf("rollback after native retirement = %#v err=%v", rolledBack, err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "fs", "sessions", "session", "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed state remains after rollback: %v", err)
	}
}

func TestFSRetireNativeRejectsActiveWriter(t *testing.T) {
	home, storeDir, nativePath := fsFixture(t, true)
	manifest, err := fold.LoadManifest(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	source, err := hashPath(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := retainCanonicalSnapshot(context.Background(), storeDir, "session", source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeCanonicalSnapshotSource(nativePath, retained); err != nil {
		t.Fatal(err)
	}
	resolver, err := pack.Open(storeDir, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: storeDir, ManifestPath: fold.ManifestPath(storeDir, "session"), Manifest: manifest, Reader: resolver, NativeSnapshot: retained,
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
	defer writer.Close()
	defer resolver.Close()

	command := NewRootCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"fs", "retire-native", "session", "--codex-home", home, "--store", storeDir, "--apply"})
	err = command.Execute()
	if err == nil || err.Error() != "cannot retire native snapshot while the session has an active writer" {
		t.Fatalf("retire-native active writer error = %v", err)
	}
	state, err := managedState(storeDir, "session")
	if err != nil || state.NativeSnapshot != retained {
		t.Fatalf("active-writer rejection changed state: %#v err=%v", state, err)
	}
	if _, err := os.Stat(retained.Path); err != nil {
		t.Fatalf("active-writer rejection removed snapshot: %v", err)
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

func TestFSBenchmarkUsesManagedVisibleBytesAfterAppendAndCompact(t *testing.T) {
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
		_ = resolver.Close()
		t.Fatal(err)
	}
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
	if _, err := writer.Append(context.Background(), []byte("{\"benchmark\":\"append\"}\n")); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}

	benchmark := func() fsctl.BenchmarkReport {
		t.Helper()
		root := NewRootCommand()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{
			"fs", "benchmark", "session", "--codex-home", home, "--store", storeDir,
			"--sequential-block-bytes", "16", "--random-block-bytes", "8", "--random-reads", "10",
			"--bypass-os-cache", "--json",
		})
		if err := root.Execute(); err != nil {
			t.Fatalf("benchmark: %v", err)
		}
		var report fsctl.BenchmarkReport
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			t.Fatalf("decode benchmark report: %v\n%s", err, output.String())
		}
		if !report.OSCacheBypassRequested {
			t.Fatal("benchmark did not preserve --bypass-os-cache")
		}
		if report.Native.Bytes != report.Virtual.Bytes {
			t.Fatalf("benchmark byte counts differ: native=%d virtual=%d", report.Native.Bytes, report.Virtual.Bytes)
		}
		return report
	}

	appended := benchmark()
	if appended.Native.Bytes <= manifest.Source.Bytes {
		t.Fatalf("benchmark ignored active delta: native=%d base=%d", appended.Native.Bytes, manifest.Source.Bytes)
	}

	executeFS(t, []string{"fs", "compact", "session", "--codex-home", home, "--store", storeDir, "--apply", "--json"})
	compacted := benchmark()
	if compacted.Native.Bytes != appended.Native.Bytes || compacted.Virtual.Bytes != appended.Virtual.Bytes {
		t.Fatalf("benchmark changed visible size across compact: appended=%+v compacted=%+v", appended, compacted)
	}
}

func TestFSDoctorUsesExplicitServiceDefinition(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	definition := filepath.Join(t.TempDir(), "isolated-service-definition")
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fs", "doctor", "--codex-home", home, "--store", storeDir,
		"--definition", definition, "--json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("fs doctor: %v", err)
	}
	var report fsctl.DoctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode fs doctor: %v\n%s", err, output.String())
	}
	for _, issue := range report.Issues {
		if issue.Component != fsctl.ComponentDaemon {
			continue
		}
		if !strings.Contains(issue.Message, definition) {
			t.Fatalf("daemon issue did not use explicit definition %q: %#v", definition, issue)
		}
		return
	}
	t.Fatalf("fs doctor did not report the missing explicit definition: %#v", report)
}

func TestFSStatusAndDoctorExposePhysicalStorageAccounting(t *testing.T) {
	home, storeDir, _ := fsFixture(t, true)
	for _, args := range [][]string{
		{"fs", "status", "--codex-home", home, "--store", storeDir, "--json"},
		{"fs", "doctor", "--codex-home", home, "--store", storeDir, "--json"},
	} {
		root := NewRootCommand()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var payload struct {
			Storage struct {
				LogicalSessionBytes int64 `json:"logical_session_bytes"`
				TotalPhysicalBytes  int64 `json:"total_physical_bytes"`
			} `json:"storage"`
			StorageLimits  storage.Limits `json:"storage_limits"`
			AvailableBytes int64          `json:"available_bytes"`
		}
		if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
			t.Fatalf("decode %v: %v\n%s", args, err, output.String())
		}
		if payload.Storage.LogicalSessionBytes <= 0 || payload.Storage.TotalPhysicalBytes <= 0 || payload.StorageLimits.MaxPhysicalBytes <= 0 || payload.AvailableBytes <= 0 {
			t.Fatalf("incomplete storage accounting for %v: %#v", args, payload)
		}
	}
}

func TestStartupStorageGCRunsOnlyAfterHealthyStoreVerification(t *testing.T) {
	_, storeDir, _ := fsFixture(t, true)
	if _, err := pack.Build(context.Background(), storeDir, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Build(context.Background(), storeDir, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	before := countPackGenerationDirectories(t, storeDir)
	result, ran, err := startupStorageGC(context.Background(), storeDir)
	if err != nil {
		t.Fatalf("startupStorageGC: %v", err)
	}
	if !ran || before != 3 || result.RemovedCount != 1 || countPackGenerationDirectories(t, storeDir) != 2 {
		t.Fatalf("healthy startup GC result: before=%d ran=%t result=%#v after=%d", before, ran, result, countPackGenerationDirectories(t, storeDir))
	}

	if _, err := pack.Build(context.Background(), storeDir, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(storeDir, "packs", "CURRENT"))
	if err != nil {
		t.Fatal(err)
	}
	packs, err := filepath.Glob(filepath.Join(storeDir, "packs", strings.TrimSpace(string(current)), "pack-*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("find current pack files: %#v err=%v", packs, err)
	}
	packPath := packs[0]
	if err := os.WriteFile(packPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = countPackGenerationDirectories(t, storeDir)
	_, ran, err = startupStorageGC(context.Background(), storeDir)
	if err != nil {
		t.Fatalf("unhealthy startupStorageGC: %v", err)
	}
	if ran || countPackGenerationDirectories(t, storeDir) != before {
		t.Fatalf("unhealthy store was mutated: ran=%t before=%d after=%d", ran, before, countPackGenerationDirectories(t, storeDir))
	}
}

func TestStartStorageMaintenanceDoesNotBlockAvailabilityOrCancelService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var diagnostics bytes.Buffer
	started := make(chan (<-chan struct{}), 1)
	go func() {
		started <- startStorageMaintenance(ctx, &diagnostics, "store", func(context.Context, string) (storage.StorageGCResult, bool, error) {
			close(entered)
			<-release
			return storage.StorageGCResult{}, true, errors.New("maintenance failed")
		})
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("storage maintenance did not start")
	}
	var done <-chan struct{}
	select {
	case done = <-started:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("storage maintenance blocked service availability")
	}
	select {
	case <-ctx.Done():
		t.Fatal("storage maintenance canceled the service")
	default:
	}

	close(release)
	released = true
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("storage maintenance did not finish")
	}
	if !strings.Contains(diagnostics.String(), "storage maintenance failed: maintenance failed") {
		t.Fatalf("diagnostics = %q", diagnostics.String())
	}
}

func TestRuntimeMemoryReclaimableUsesOnlyUnreleasedIdleHeap(t *testing.T) {
	if runtimeMemoryReclaimable(runtime.MemStats{HeapIdle: 128 << 20, HeapReleased: 80 << 20}, 64<<20) {
		t.Fatal("reclaimed below-threshold idle heap")
	}
	if !runtimeMemoryReclaimable(runtime.MemStats{HeapIdle: 160 << 20, HeapReleased: 80 << 20}, 64<<20) {
		t.Fatal("did not reclaim above-threshold idle heap")
	}
	if runtimeMemoryReclaimable(runtime.MemStats{HeapIdle: 64 << 20, HeapReleased: 96 << 20}, 1) {
		t.Fatal("underflowed released heap accounting")
	}
}

func countPackGenerationDirectories(t *testing.T, store string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			count++
		}
	}
	return count
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
	dbPath := filepath.Join(home, "state_5.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	archivedValue := 0
	if archived {
		archivedValue = 1
	}
	_, err = db.Exec(`update threads set archived = ?, id = 'session' where id = 'fixture'`, archivedValue)
	_ = db.Close()
	if err != nil {
		t.Fatalf("update fixture session state: %v", err)
	}
	session := codex.Session{ID: "session", RolloutPath: nativePath, Archived: archived}
	if _, err := fold.Fold(context.Background(), toFoldSession(session), fold.FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 8}); err != nil {
		t.Fatalf("fold fixture: %v", err)
	}
	if _, err := pack.Build(context.Background(), storeDir, pack.BuildOptions{}); err != nil {
		t.Fatalf("pack fixture: %v", err)
	}
	return home, storeDir, nativePath
}

func addEnrollmentFixtureSession(t *testing.T, home string, sessionID string, updatedAt int64) string {
	t.Helper()
	rolloutPath := filepath.Join(home, sessionID+".jsonl")
	if err := os.WriteFile(rolloutPath, []byte("{\"type\":\"session_meta\"}\n{\"value\":\""+sessionID+"-repeated-field-value\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, insertErr := db.Exec(`insert into threads values (?, ?, '/workspace', ?, 'provider', 'model', ?, 1, '')`, sessionID, sessionID, rolloutPath, updatedAt)
	closeErr := db.Close()
	if err := errors.Join(insertErr, closeErr); err != nil {
		t.Fatal(err)
	}
	return rolloutPath
}

func saveEnrollmentObservations(t *testing.T, storeDir string, sessions map[string]string) {
	t.Helper()
	observations := make(enroll.Observations, len(sessions))
	for sessionID, rolloutPath := range sessions {
		info, err := os.Stat(rolloutPath)
		if err != nil {
			t.Fatal(err)
		}
		observations[sessionID] = enroll.Observation{
			Path: rolloutPath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(),
			StableSinceUnixNano: time.Now().Add(-time.Hour).UnixNano(),
		}
	}
	if err := enroll.SaveObservations(enrollmentObservationPath(storeDir), observations); err != nil {
		t.Fatal(err)
	}
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

func emulateCanonicalRetirement(storeDir string, sessionID string, mountedTarget string, nativeTarget string) <-chan error {
	done := make(chan error, 1)
	go func() {
		stateDirectory := filepath.Join(storeDir, "fs", "sessions", sessionID)
		requestPath := filepath.Join(stateDirectory, retirementRequestFilename)
		acknowledgementPath := filepath.Join(stateDirectory, retirementAcknowledgementFilename)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(stateDirectory); err != nil {
				done <- fmt.Errorf("managed state moved before retirement request: %w", err)
				return
			}
			request, err := os.ReadFile(requestPath)
			if err == nil {
				data, readErr := os.ReadFile(nativeTarget)
				if readErr == nil {
					readErr = os.WriteFile(mountedTarget, data, 0o600)
				}
				if readErr == nil {
					readErr = os.WriteFile(acknowledgementPath, request, 0o600)
				}
				done <- readErr
				return
			}
			if !errors.Is(err, os.ErrNotExist) {
				done <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		done <- errors.New("retirement request was not created")
	}()
	return done
}

func emulateCanonicalRetirementGeneration(t *testing.T, storeDir string, sessionID string, mountedTarget string, nativeTarget string, expectedGeneration uint64) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		stateDirectory := filepath.Join(storeDir, "fs", "sessions", sessionID)
		requestPath := filepath.Join(stateDirectory, retirementRequestFilename)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(requestPath)
			if err == nil {
				var request retirementControl
				if err := json.Unmarshal(data, &request); err != nil {
					done <- err
					return
				}
				if request.Generation != expectedGeneration {
					rejection := request
					rejection.Error = fmt.Sprintf("retirement generation = %d, want recovered %d", request.Generation, expectedGeneration)
					if err := writeRetirementAcknowledgement(storeDir, sessionID, rejection); err != nil {
						done <- err
						return
					}
					for time.Now().Before(deadline) {
						if _, err := os.Stat(requestPath); !errors.Is(err, os.ErrNotExist) {
							time.Sleep(5 * time.Millisecond)
							continue
						}
						state, err := managedState(storeDir, sessionID)
						if err != nil || state.Generation <= expectedGeneration {
							time.Sleep(5 * time.Millisecond)
							continue
						}
						native, err := os.ReadFile(nativeTarget)
						if err == nil {
							err = os.WriteFile(mountedTarget, native, 0o600)
						}
						if err == nil {
							err = writeMountAcknowledgement(storeDir, sessionID, state.Generation, request.Route)
						}
						done <- err
						return
					}
					done <- errors.New("managed route was not republished after stale retirement request")
					return
				}
				native, err := os.ReadFile(nativeTarget)
				if err == nil {
					err = os.WriteFile(mountedTarget, native, 0o600)
				}
				if err == nil {
					err = writeRetirementAcknowledgement(storeDir, sessionID, request)
				}
				done <- err
				return
			}
			if !errors.Is(err, os.ErrNotExist) {
				done <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		done <- errors.New("retirement request was not created")
	}()
	return done
}

func readMountedFilesystemFile(t *testing.T, filesystem *mountfs.Filesystem, route string, size int) []byte {
	t.Helper()
	handle, errno := filesystem.Open(route, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open mounted route %s: %v", route, errno)
	}
	defer filesystem.Release(handle)
	data := make([]byte, size)
	n, errno := filesystem.Read(handle, data, 0)
	if errno != 0 {
		t.Fatalf("read mounted route %s: %v", route, errno)
	}
	return data[:n]
}

func allowFixtureMount(t *testing.T) {
	t.Helper()
	previous := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = previous })
	allowEnrollmentNamespaceReadiness(t)
}

func allowEnrollmentNamespaceReadiness(t *testing.T) {
	t.Helper()
	previous := enrollmentCanonicalNamespaceReadinessProbe
	enrollmentCanonicalNamespaceReadinessProbe = func(string, string, string) canonicalNamespaceReadiness {
		return canonicalNamespaceReadiness{Active: true, Ready: true}
	}
	t.Cleanup(func() { enrollmentCanonicalNamespaceReadinessProbe = previous })
}

func TestCanonicalNativePassthroughProbeRequiresEveryNativeRoute(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "mount")
	nativeRoot := filepath.Join(root, "native")
	for _, base := range []string{mount, nativeRoot} {
		for _, namespace := range []string{"sessions", "archived_sessions"} {
			if err := os.MkdirAll(filepath.Join(base, namespace, "2026", "07"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	nativeFile := filepath.Join(nativeRoot, "sessions", "2026", "07", "unmanaged.jsonl")
	if err := os.WriteFile(nativeFile, []byte("native\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeCanonicalNativePassthrough(mount, nativeRoot); err == nil {
		t.Fatal("missing mounted native route passed readiness")
	}
	mountedFile := filepath.Join(mount, "sessions", "2026", "07", "unmanaged.jsonl")
	if err := os.WriteFile(mountedFile, []byte("mounted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeCanonicalNativePassthrough(mount, nativeRoot); err != nil {
		t.Fatalf("complete native passthrough failed readiness: %v", err)
	}
}
