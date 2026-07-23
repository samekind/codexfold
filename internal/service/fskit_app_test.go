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
}

func TestFSKitHostLauncherRejectsNonAppAndRelativePaths(t *testing.T) {
	for _, path := range []string{"CodexFoldFSKit.app", filepath.Join(t.TempDir(), "CodexFoldFSKit")} {
		if _, err := FSKitHostLauncherPath(path); err == nil {
			t.Fatalf("FSKitHostLauncherPath(%q) succeeded", path)
		}
	}
}
