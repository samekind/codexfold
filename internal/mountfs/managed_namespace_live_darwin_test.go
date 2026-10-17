//go:build darwin

package mountfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This uses the installed FSKit module with a NEW resource/mount and the actual
// installed jstarctl. It never points jstarctl at the production SQLite file.
func TestManagedNamespaceMountedJstarctlRebind(t *testing.T) {
	if os.Getenv("CODEXFOLD_NAMESPACE_LIVE") != "1" {
		t.Skip("explicit isolated native FSKit run required")
	}
	f, store, _, route, original := namespaceFixture(t)
	user, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	resource, err := os.MkdirTemp(filepath.Join(user, "Library", "Group Containers", "group.vip.jstar.codexfold"), "r-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(resource)
	mount, err := os.MkdirTemp("/private/tmp", "cfd-rebind-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(mount)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeNativeFSKit(ctx, f, NativeFSKitServerOptions{ResourcePath: resource, SocketPath: filepath.Join(resource, "daemon.sock"), MountPoint: mount})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("isolated server shutdown timeout")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(resource, "daemon.sock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mountCtx, mountCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer mountCancel()
	if output, err := exec.CommandContext(mountCtx, "/sbin/mount", "-F", "-t", "codexfoldnative", resource, mount).CombinedOutput(); err != nil {
		t.Fatalf("isolated mount: %v %s", err, output)
	}
	defer func() {
		unmountCtx, unmountCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer unmountCancel()
		if output, err := exec.CommandContext(unmountCtx, "/sbin/umount", mount).CombinedOutput(); err != nil {
			t.Errorf("isolated unmount: %v %s", err, output)
		}
	}()
	path := nativePathFromRoot(mount, route)
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("folded mounted source: %v", err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(home, ".codex", "state_5.sqlite")
	sql := "CREATE TABLE threads (id TEXT PRIMARY KEY,title TEXT,cwd TEXT,rollout_path TEXT,model_provider TEXT,model TEXT,updated_at INTEGER,archived INTEGER,git_branch TEXT); INSERT INTO threads VALUES ('user-thread','compatibility fixture','" + home + "','" + path + "','openai','',1,0,'');"
	if output, err := exec.Command("/usr/bin/sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("isolated sqlite: %v %s", err, output)
	}
	rebindCtx, rebindCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer rebindCancel()
	cmd := exec.CommandContext(rebindCtx, filepath.Join(user, ".local", "bin", "jstarctl"), "codex", "rebind", "main", "user-thread", "--force", "--json")
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "HOME=") && !strings.HasPrefix(env, "CODEX_HOME=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "CODEX_HOME="+filepath.Join(home, ".codex"))
	start := time.Now()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual jstarctl rebind: %v\n%s", err, output)
	}
	t.Logf("actual jstarctl rebind took %s; isolated store=%s; result=%s", time.Since(start), store, output)
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(got, []byte(`"model_provider":"main"`)) || !bytes.Contains(got, []byte("original history")) {
		t.Fatalf("rebound mounted content: %v %s", err, got)
	}
	backups, err := filepath.Glob(path + ".*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("mounted backups: %v %v", backups, err)
	}
	if got, err := os.ReadFile(backups[0]); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("mounted backup differs: %v", err)
	}
	oldBytes := make([]byte, len(original))
	if n, err := old.ReadAt(oldBytes, 0); err != nil || n != len(original) || !bytes.Equal(oldBytes, original) {
		t.Fatalf("old mounted descriptor changed: %v", err)
	}
	provider, err := exec.Command("/usr/bin/sqlite3", db, "SELECT model_provider FROM threads WHERE id='user-thread';").Output()
	if err != nil || strings.TrimSpace(string(provider)) != "main" {
		t.Fatalf("isolated sqlite provider=%s err=%v", provider, err)
	}
	// Append after rebind is the operation Codex itself needs next.
	writer, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	tail := []byte("{\"after_rebind\":true}\n")
	_, writeErr := writer.Write(tail)
	err = errors.Join(writeErr, writer.Sync(), writer.Close())
	if err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, append(got, tail...)) {
		t.Fatalf("mounted append/read after rebind: %v", err)
	}
	if backup, err := os.ReadFile(backups[0]); err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("append changed backup: %v", err)
	}
	// Exercise the native-to-folded cutover through the same mounted inode
	// cache the real client uses, not only the in-memory adapter.
	if err := f.FoldNativeReplacements(context.Background(), namespacePrepare(t, store)); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, append(got, tail...)) {
		t.Fatalf("mounted read after background fold: %v", err)
	}
	if backup, err := os.ReadFile(backups[0]); err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("background fold changed mounted backup: %v", err)
	}
	if n, err := old.ReadAt(oldBytes, 0); err != nil || n != len(original) || !bytes.Equal(oldBytes, original) {
		t.Fatalf("background fold changed old mounted descriptor: %v", err)
	}
	writer, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	secondTail := []byte("{\"after_background_fold\":true}\n")
	_, writeErr = writer.Write(secondTail)
	if err := errors.Join(writeErr, writer.Sync(), writer.Close()); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, append(append(got, tail...), secondTail...)) {
		t.Fatalf("mounted append after background fold: %v", err)
	}
}
