package service

import (
	"encoding/json"
	"github.com/samekind/codexfold/internal/buildid"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsCoreImageBinding(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "codexfold.exe")
	if err := os.WriteFile(base, []byte("frontend"), 0600); err != nil {
		t.Fatal(err)
	}
	if path, err := configuredWindowsCoreImage(base); err != nil || path != base {
		t.Fatal(path, err)
	}
	core := filepath.Join(root, "Core")
	_ = os.Mkdir(core, 0700)
	image := filepath.Join(root, "new.exe")
	_ = os.WriteFile(image, []byte("new-engine"), 0600)
	sha, _ := buildid.FileSHA256(image)
	target := filepath.Join(core, sha+".exe")
	_ = os.Rename(image, target)
	binding, _ := json.Marshal(struct {
		Version int
		SHA256  string
	}{1, sha})
	_ = os.WriteFile(filepath.Join(core, "current.json"), binding, 0600)
	if path, err := configuredWindowsCoreImage(base); err != nil || path != target {
		t.Fatal(path, err)
	}
	_ = os.WriteFile(target, []byte("changed"), 0600)
	if _, err := configuredWindowsCoreImage(base); err == nil {
		t.Fatal("unverified image accepted")
	}
}

func TestWindowsCoreDefinitionPaths(t *testing.T) {
	root := t.TempDir()
	definition := filepath.Join(root, "service.json")
	data, _ := json.Marshal(WindowsConfig{Version: 1, ServiceName: "com.codexfold.fs", BinaryPath: filepath.Join(root, "core.exe"),
		Arguments:  []string{"fs", "serve", "--store=" + root, "--mount", filepath.Join(root, "mount"), "--native-root", filepath.Join(root, "native")},
		StdoutPath: filepath.Join(root, "out.log"), StderrPath: filepath.Join(root, "err.log")})
	if err := os.WriteFile(definition, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		load func(Platform, string) (string, error)
		want string
	}{{DefinitionStore, root}, {DefinitionMountPoint, filepath.Join(root, "mount")}, {DefinitionNativeRoot, filepath.Join(root, "native")}} {
		value, err := item.load(PlatformWindows, definition)
		if err != nil || value != item.want {
			t.Fatal(value, err)
		}
	}
}

func TestWindowsCoreOfflineUpdateRestoresEngineBinding(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "codexfold.exe")
	core := filepath.Join(root, "Core")
	_ = os.Mkdir(core, 0700)
	_ = os.WriteFile(base, []byte("old-engine"), 0600)
	oldSHA, _ := buildid.FileSHA256(base)
	_ = os.WriteFile(filepath.Join(core, oldSHA+".exe"), []byte("old-engine"), 0600)
	previous, _ := json.Marshal(struct {
		Version int
		SHA256  string
	}{1, oldSHA})
	_ = os.WriteFile(filepath.Join(core, "current.json"), previous, 0600)
	_ = os.WriteFile(base, []byte("new-engine"), 0600)
	rollback, err := PrepareWindowsCoreOfflineUpdate(base)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := configuredWindowsCoreImage(base)
	if err != nil {
		t.Fatal(err)
	}
	newSHA, _ := buildid.FileSHA256(selected)
	if newSHA == oldSHA {
		t.Fatal("offline update kept old engine")
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	selected, err = configuredWindowsCoreImage(base)
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := buildid.FileSHA256(selected)
	if restored != oldSHA {
		t.Fatal("rollback changed old image")
	}
}
