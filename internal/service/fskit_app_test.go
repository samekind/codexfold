package service

import (
	"path/filepath"
	"testing"
)

func TestFSKitManagedPathsUseStableAppAndAppGroupLocations(t *testing.T) {
	home := filepath.Join(t.TempDir(), "user")
	app := DefaultFSKitAppPath(home)
	if app != filepath.Join(home, "Applications", FSKitAppBundleName) {
		t.Fatalf("default app path = %q", app)
	}
	launcher, err := FSKitHostLauncherPath(app)
	if err != nil {
		t.Fatal(err)
	}
	if launcher != filepath.Join(app, "Contents", "MacOS", FSKitHostExecutableName) {
		t.Fatalf("launcher path = %q", launcher)
	}
	resource := DefaultFSKitResourcePath(home)
	if resource != filepath.Join(home, "Library", "Group Containers", FSKitAppGroupIdentifier, FSKitResourceDirectoryName) {
		t.Fatalf("resource path = %q", resource)
	}
	if status := FSKitStatusPath(resource, "daemon"); status != filepath.Join(home, "Library", "Group Containers", FSKitAppGroupIdentifier, "status", "daemon.json") {
		t.Fatalf("status path = %q", status)
	}
}

func TestFSKitStatusPathFindsAppGroupAboveNestedResource(t *testing.T) {
	group := filepath.Join(t.TempDir(), FSKitAppGroupIdentifier)
	resource := filepath.Join(group, "nested", "native-fskit")
	if status := FSKitStatusPath(resource, "supervisor"); status != filepath.Join(resource, "status", "supervisor.json") {
		t.Fatalf("status path = %q", status)
	}
}

func TestFSKitStatusPathScopesIndependentResources(t *testing.T) {
	group := filepath.Join(t.TempDir(), FSKitAppGroupIdentifier)
	first := filepath.Join(group, "acceptance-one")
	second := filepath.Join(group, "acceptance-two")
	if FSKitStatusPath(first, "daemon") == FSKitStatusPath(second, "daemon") {
		t.Fatal("independent FSKit resources shared one daemon status path")
	}
	if status := FSKitStatusPath(first, "daemon"); status != filepath.Join(first, "status", "daemon.json") {
		t.Fatalf("scoped status path = %q", status)
	}
}

func TestFSKitHostLauncherRejectsNonAppAndRelativePaths(t *testing.T) {
	for _, path := range []string{"CodexFoldFSKit.app", filepath.Join(t.TempDir(), "CodexFoldFSKit")} {
		if _, err := FSKitHostLauncherPath(path); err == nil {
			t.Fatalf("FSKitHostLauncherPath(%q) succeeded", path)
		}
	}
}
