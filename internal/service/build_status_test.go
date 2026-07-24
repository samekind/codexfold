package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/buildid"
	"github.com/samekind/codexfold/internal/mountid"
)

func TestInspectBuildMatchesRunningMountAndConfiguredBinary(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "codexfold")
	if err := os.WriteFile(binary, []byte("candidate-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(root, "com.codexfold.fs.plist")
	plist, err := RenderLaunchd(Options{
		Label: "com.codexfold.fs", BinaryPath: binary, CodexHome: filepath.Join(root, "home"),
		StoreDir: filepath.Join(root, "store"), MountPoint: filepath.Join(root, "mount"),
		StdoutPath: filepath.Join(root, "stdout.log"), StderrPath: filepath.Join(root, "stderr.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definition, plist, 0o600); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(root, "mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := buildid.FileSHA256(binary)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := mountid.New(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, mountid.Path), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	status := InspectBuild(PlatformLaunchd, definition, mount)
	if !status.Healthy || status.RunningBuildSHA256 != digest || status.ConfiguredBuildSHA256 != digest || status.ConfiguredBinaryPath != binary {
		t.Fatalf("build status = %#v", status)
	}
}

func TestInspectBuildRejectsStaleRunningDaemon(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "codexfold")
	if err := os.WriteFile(binary, []byte("new-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(root, "com.codexfold.fs.plist")
	plist, err := RenderLaunchd(Options{
		Label: "com.codexfold.fs", BinaryPath: binary, CodexHome: filepath.Join(root, "home"),
		StoreDir: filepath.Join(root, "store"), MountPoint: filepath.Join(root, "mount"),
		StdoutPath: filepath.Join(root, "stdout.log"), StderrPath: filepath.Join(root, "stderr.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definition, plist, 0o600); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(root, "mount")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := mountid.New(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, mountid.Path), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	status := InspectBuild(PlatformLaunchd, definition, mount)
	if status.Healthy || !strings.Contains(status.Error, "does not match") {
		t.Fatalf("stale build status = %#v", status)
	}
}

func TestDefinitionBinaryParsesEveryRenderedPlatform(t *testing.T) {
	root := t.TempDir()
	options := Options{
		Label: "com.codexfold.fs", BinaryPath: filepath.Join(root, "Codex Fold"),
		CodexHome: filepath.Join(root, "home"), StoreDir: filepath.Join(root, "store"),
		MountPoint: filepath.Join(root, "mount"), StdoutPath: filepath.Join(root, "stdout.log"),
		StderrPath: filepath.Join(root, "stderr.log"),
	}
	for _, platform := range []Platform{PlatformLaunchd, PlatformSystemd, PlatformWindows} {
		definition, err := RenderDefinition(platform, options)
		if err != nil {
			t.Fatalf("render %s: %v", platform, err)
		}
		path := filepath.Join(root, string(platform)+".definition")
		if err := os.WriteFile(path, definition, 0o600); err != nil {
			t.Fatal(err)
		}
		binary, err := DefinitionBinary(platform, path)
		if err != nil || binary != options.BinaryPath {
			t.Fatalf("definition binary %s = %q err=%v", platform, binary, err)
		}
	}
}

func TestDefinitionFrontendParsesNativeFSKitLaunchdArguments(t *testing.T) {
	root := t.TempDir()
	resource := filepath.Join(root, "store", "fs", "native-fskit.resource")
	launcher := filepath.Join(root, "CodexFoldFSKit.app", "Contents", "MacOS", "CodexFoldFSKit")
	binary := filepath.Join(root, "codexfold")
	definition, err := RenderLaunchd(Options{
		Label: "com.codexfold.fs", BinaryPath: binary, LauncherPath: launcher,
		CodexHome: filepath.Join(root, "home"), StoreDir: filepath.Join(root, "store"),
		MountPoint: filepath.Join(root, "mount"), StdoutPath: filepath.Join(root, "stdout.log"),
		StderrPath: filepath.Join(root, "stderr.log"), CanonicalNamespace: true,
		NativeRoot: filepath.Join(root, "native"), Frontend: "native-fskit", FSKitResource: resource,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "com.codexfold.fs.plist")
	if err := os.WriteFile(path, definition, 0o600); err != nil {
		t.Fatal(err)
	}
	frontend, err := DefinitionFrontend(PlatformLaunchd, path)
	if err != nil || frontend != "native-fskit" {
		t.Fatalf("frontend = %q err=%v", frontend, err)
	}
	gotResource, err := DefinitionFSKitResource(PlatformLaunchd, path)
	if err != nil || gotResource != resource {
		t.Fatalf("resource = %q err=%v", gotResource, err)
	}
	gotStore, err := DefinitionStore(PlatformLaunchd, path)
	if err != nil || gotStore != filepath.Join(root, "store") {
		t.Fatalf("store = %q err=%v", gotStore, err)
	}
	gotNativeRoot, err := DefinitionNativeRoot(PlatformLaunchd, path)
	if err != nil || gotNativeRoot != filepath.Join(root, "native") {
		t.Fatalf("native root = %q err=%v", gotNativeRoot, err)
	}
	label, err := DefinitionLabel(PlatformLaunchd, path)
	if err != nil || label != "com.codexfold.fs" {
		t.Fatalf("label = %q err=%v", label, err)
	}
	configuredBinary, err := DefinitionBinary(PlatformLaunchd, path)
	if err != nil || configuredBinary != binary {
		t.Fatalf("wrapped definition binary = %q err=%v", configuredBinary, err)
	}
	configuredLauncher, err := DefinitionLauncher(PlatformLaunchd, path)
	if err != nil || configuredLauncher != launcher {
		t.Fatalf("wrapped definition launcher = %q err=%v", configuredLauncher, err)
	}
}
