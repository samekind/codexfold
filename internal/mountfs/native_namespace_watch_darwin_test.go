//go:build darwin

package mountfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchNativeNamespaceBumpsVersionForExternalEntryChanges(t *testing.T) {
	root := t.TempDir()
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(root, namespace), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- filesystem.WatchNativeNamespace(ctx) }()
	initial := filesystem.NamespaceVersion()
	readyDeadline := time.Now().Add(5 * time.Second)
	for filesystem.NamespaceVersion() == initial && time.Now().Before(readyDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if filesystem.NamespaceVersion() == initial {
		t.Fatal("native namespace watcher did not become ready")
	}
	baseline := filesystem.NamespaceVersion()
	target := filepath.Join(root, "sessions", "external.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for filesystem.NamespaceVersion() == baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if filesystem.NamespaceVersion() == baseline {
		t.Fatal("external namespace creation did not bump the version")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native namespace watcher did not stop")
	}
}
