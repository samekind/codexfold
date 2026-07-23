//go:build darwin

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCompareFSKitBundleVersions(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{left: "2", right: "1", want: 1},
		{left: "1.0.1", right: "1", want: 1},
		{left: "1", right: "1.0", want: 0},
		{left: "1.9", right: "2", want: -1},
	}
	for _, test := range tests {
		got, err := compareFSKitBundleVersions(test.left, test.right)
		if err != nil {
			t.Fatalf("compare %q and %q: %v", test.left, test.right, err)
		}
		if got != test.want {
			t.Fatalf("compare %q and %q = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestCompareFSKitBundleVersionsRejectsInvalidInput(t *testing.T) {
	for _, version := range []string{"", "1.2.3.4", "1.beta"} {
		if _, err := compareFSKitBundleVersions(version, "1"); err == nil {
			t.Fatalf("invalid version %q was accepted", version)
		}
	}
}

func TestParseFSKitModulePathsIncludesDuplicateRegistrations(t *testing.T) {
	output := []byte(`
+    vip.jstar.codexfold.fskitprofileprobe.module(0.1.1)	OLD	2026-07-22 08:23:53 +0000	/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex
+    vip.jstar.codexfold.fskitprofileprobe.module(0.1.1)	CURRENT	2026-07-22 09:24:23 +0000	/Users/test/Applications/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex
+    vip.jstar.codexfold.fskitprofileprobe.module(0.1.1)	DUPLICATE	2026-07-22 09:24:24 +0000	/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex
 (3 plug-ins)
`)
	want := []string{
		"/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
		"/Users/test/Applications/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
	}
	if got := parseFSKitModulePaths(output); !reflect.DeepEqual(got, want) {
		t.Fatalf("module paths = %#v, want %#v", got, want)
	}
}

func TestSameFSKitModulePathsIgnoresOrderAndDuplicateEntries(t *testing.T) {
	left := []string{"/tmp/current.appex", "/tmp/old.appex", "/tmp/current.appex"}
	right := []string{"/tmp/old.appex", "/tmp/current.appex"}
	if !sameFSKitModulePaths(left, right) {
		t.Fatalf("module path sets differ: left=%v right=%v", left, right)
	}
	if sameFSKitModulePaths(left, []string{"/tmp/current.appex"}) {
		t.Fatal("different module path sets were treated as equal")
	}
}

func TestStaleFSKitModulePathsKeepsInstalledModule(t *testing.T) {
	target := "/Users/test/Applications/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex"
	paths := []string{
		"/private/tmp/candidate/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
		target,
		"/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
		target,
	}
	want := []string{
		"/private/tmp/candidate/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
		"/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
	}
	if got := staleFSKitModulePaths(paths, target); !reflect.DeepEqual(got, want) {
		t.Fatalf("stale module paths = %#v, want %#v", got, want)
	}
}

func TestUnregisterFSKitAppRegistrationUsesLaunchServicesOnly(t *testing.T) {
	original := runFSKitLaunchServicesCommand
	t.Cleanup(func() { runFSKitLaunchServicesCommand = original })
	var got []string
	runFSKitLaunchServicesCommand = func(_ context.Context, args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return nil, nil
	}
	app := "/private/tmp/candidate/CodexFoldFSKit.app"
	if err := unregisterFSKitAppRegistration(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if want := []string{"-u", app}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LaunchServices cleanup args = %#v, want %#v", got, want)
	}
}

func TestFSKitParentAppPath(t *testing.T) {
	module := "/private/tmp/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex"
	if got, ok := fsKitParentAppPath(module); !ok || got != "/private/tmp/CodexFoldFSKit.app" {
		t.Fatalf("parent app = %q ok=%t", got, ok)
	}
	if _, ok := fsKitParentAppPath("/private/tmp/CodexFoldFSKitModule.appex"); ok {
		t.Fatal("module outside an app bundle unexpectedly had a parent app")
	}
}

func TestFSKitAppContentsSwapPreservesBundleRootAndRollsBack(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "CodexFoldFSKit.app")
	stageRoot := filepath.Join(root, "stage")
	stagePath := filepath.Join(stageRoot, filepath.Base(target))
	writeFSKitAppTestFile(t, filepath.Join(target, "Contents", "old.txt"), "old")
	writeFSKitAppTestFile(t, filepath.Join(stagePath, "Contents", "new.txt"), "new")

	attribute := "com.codexfold.test-root"
	value := []byte("preserve-this-root-xattr")
	if err := unix.Setxattr(target, attribute, value, 0); err != nil {
		t.Fatalf("set root xattr: %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	transaction := &darwinFSKitAppTransaction{
		target: target, stageRoot: stageRoot, stagePath: stagePath, changed: true,
	}
	if err := transaction.promoteStagedApp(); err != nil {
		t.Fatalf("promote staged app: %v", err)
	}
	assertFSKitAppRootUnchanged(t, target, before, attribute, value)
	assertFSKitAppTestFile(t, filepath.Join(target, "Contents", "new.txt"), "new")
	assertFSKitAppTestFile(t, filepath.Join(stagePath, "Contents", "old.txt"), "old")

	if err := transaction.rollbackStagedApp(); err != nil {
		t.Fatalf("rollback staged app: %v", err)
	}
	assertFSKitAppRootUnchanged(t, target, before, attribute, value)
	assertFSKitAppTestFile(t, filepath.Join(target, "Contents", "old.txt"), "old")
	if _, err := os.Stat(filepath.Join(target, "Contents", "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new contents remain after rollback: %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit cleanup: %v", err)
	}
	if _, err := os.Stat(stageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging root remains after commit: %v", err)
	}
}

func TestFSKitAppContentsSwapFirstInstallRemovesOnlyCandidateOnRollback(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "CodexFoldFSKit.app")
	stageRoot := filepath.Join(root, "stage")
	stagePath := filepath.Join(stageRoot, filepath.Base(target))
	writeFSKitAppTestFile(t, filepath.Join(stagePath, "Contents", "new.txt"), "new")
	transaction := &darwinFSKitAppTransaction{
		target: target, stageRoot: stageRoot, stagePath: stagePath, changed: true,
	}
	if err := transaction.promoteStagedApp(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !transaction.appInstalled || transaction.hadTarget {
		t.Fatalf("first-install state = %#v", transaction)
	}
	assertFSKitAppTestFile(t, filepath.Join(target, "Contents", "new.txt"), "new")
	if err := transaction.rollbackStagedApp(); err != nil {
		t.Fatalf("rollback first install: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate app remains after rollback: %v", err)
	}
}

func writeFSKitAppTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFSKitAppTestFile(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("file %s = %q, want %q", path, got, want)
	}
}

func assertFSKitAppRootUnchanged(t *testing.T, path string, before os.FileInfo, attribute string, want []byte) {
	t.Helper()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatalf("app bundle root inode changed: before=%#v after=%#v", before.Sys(), after.Sys())
	}
	got := make([]byte, len(want))
	n, err := unix.Getxattr(path, attribute, got)
	if err != nil {
		t.Fatalf("read root xattr: %v", err)
	}
	if string(got[:n]) != string(want) {
		t.Fatalf("root xattr = %q, want %q", got[:n], want)
	}
}
