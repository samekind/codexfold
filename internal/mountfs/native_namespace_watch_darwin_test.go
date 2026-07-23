//go:build darwin

package mountfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestNativeNamespaceSnapshotRefreshesOnlyVisibleNewAndReplacedEntries(t *testing.T) {
	root := t.TempDir()
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(root, namespace), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := scanNativeNamespace(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "sessions", "2099", "12", "31")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "external.jsonl")
	if err := os.WriteFile(target, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "._external.jsonl"), []byte("sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	hiddenDirectory := filepath.Join(directory, ".hidden")
	if err := os.MkdirAll(hiddenDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hiddenDirectory, "ignored.jsonl"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := scanNativeNamespace(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []nativeNamespaceRefreshEntry{
		{route: "/sessions/2099", directory: true},
		{route: "/sessions/2099/12", directory: true},
		{route: "/sessions/2099/12/31", directory: true},
		{route: "/sessions/2099/12/31/external.jsonl", directory: false},
	}
	if got := nativeNamespaceRefreshDelta(baseline, current); !slices.Equal(got, want) {
		t.Fatalf("new namespace refresh entries = %#v, want %#v", got, want)
	}

	if err := os.WriteFile(target, []byte("same-inode-update\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modified, err := scanNativeNamespace(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want = []nativeNamespaceRefreshEntry{{route: "/sessions/2099/12/31/external.jsonl", directory: false}}
	if got := nativeNamespaceRefreshDelta(current, modified); !slices.Equal(got, want) {
		t.Fatalf("same-inode data refresh entries = %#v, want %#v", got, want)
	}

	oldTarget := filepath.Join(root, "old-external.jsonl")
	if err := os.Rename(target, oldTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced, err := scanNativeNamespace(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want = []nativeNamespaceRefreshEntry{{route: "/sessions/2099/12/31/external.jsonl", directory: false}}
	if got := nativeNamespaceRefreshDelta(modified, replaced); !slices.Equal(got, want) {
		t.Fatalf("replacement namespace refresh entries = %#v, want %#v", got, want)
	}
}

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
	sessionsBefore, errno := filesystem.Getattr("/sessions")
	if errno != 0 {
		t.Fatalf("sessions Getattr before change errno=%v", errno)
	}
	archivedBefore, errno := filesystem.Getattr("/archived_sessions")
	if errno != 0 {
		t.Fatalf("archived Getattr before change errno=%v", errno)
	}
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
	sessionsAfter, errno := filesystem.Getattr("/sessions")
	if errno != 0 {
		t.Fatalf("sessions Getattr after change errno=%v", errno)
	}
	if sessionsAfter.ObjectID != sessionsBefore.ObjectID {
		t.Fatalf("changed directory replaced stable object ID %q with %q", sessionsBefore.ObjectID, sessionsAfter.ObjectID)
	}
	if sessionsAfter.DirectoryGeneration <= sessionsBefore.DirectoryGeneration {
		t.Fatalf("changed directory generation did not advance: before=%d after=%d", sessionsBefore.DirectoryGeneration, sessionsAfter.DirectoryGeneration)
	}
	archivedAfter, errno := filesystem.Getattr("/archived_sessions")
	if errno != 0 {
		t.Fatalf("archived Getattr after change errno=%v", errno)
	}
	if archivedAfter.ObjectID != archivedBefore.ObjectID {
		t.Fatalf("unrelated directory identity changed from %q to %q", archivedBefore.ObjectID, archivedAfter.ObjectID)
	}

	baseline = filesystem.NamespaceVersion()
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("{\"updated\":true}\n")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for filesystem.NamespaceVersion() == baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if filesystem.NamespaceVersion() == baseline {
		t.Fatal("same-inode external append did not bump the version")
	}
	attribute, errno := filesystem.Getattr("/sessions/external.jsonl")
	if errno != 0 || attribute.Size != int64(len("{}\n{\"updated\":true}\n")) {
		t.Fatalf("same-inode external append size=%d errno=%v", attribute.Size, errno)
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

func TestRetryNativeNamespaceRefreshesKeepsTransientFailuresPending(t *testing.T) {
	pending := make(map[string]nativeNamespaceRefreshEntry)
	mergeNativeNamespaceRefreshEntries(pending, []nativeNamespaceRefreshEntry{
		{route: "/sessions/2099/12/31/external.jsonl", directory: false},
		{route: "/sessions/2099", directory: true},
	})

	var firstAttempt []string
	retryNativeNamespaceRefreshes(pending, func(route string, directory bool) error {
		firstAttempt = append(firstAttempt, route)
		if route == "/sessions/2099" {
			return errors.New("mount is temporarily unavailable")
		}
		return nil
	})
	wantFirstAttempt := []string{"/sessions/2099", "/sessions/2099/12/31/external.jsonl"}
	if !slices.Equal(firstAttempt, wantFirstAttempt) {
		t.Fatalf("first refresh order = %v, want %v", firstAttempt, wantFirstAttempt)
	}
	if len(pending) != 1 || !pending["/sessions/2099"].directory {
		t.Fatalf("pending refreshes after transient failure = %#v", pending)
	}

	retryNativeNamespaceRefreshes(pending, func(route string, directory bool) error {
		if route != "/sessions/2099" || !directory {
			t.Fatalf("retried refresh = %q directory=%t", route, directory)
		}
		return nil
	})
	if len(pending) != 0 {
		t.Fatalf("pending refreshes after recovery = %#v", pending)
	}
}
