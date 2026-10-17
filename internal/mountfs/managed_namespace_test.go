package mountfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/vfs"
)

func namespaceFixture(t *testing.T) (*Filesystem, string, string, string, []byte) {
	t.Helper()
	root := t.TempDir()
	store, native := filepath.Join(root, "store"), filepath.Join(root, "native")
	route := "/sessions/2026/10/15/rollout-user-thread.jsonl"
	if err := os.MkdirAll(filepath.Dir(nativePathFromRoot(native, route)), 0700); err != nil {
		t.Fatal(err)
	}
	source := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"user-thread\",\"model_provider\":\"openai\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"original history\"}}\n")
	if mb, _ := strconv.Atoi(os.Getenv("CODEXFOLD_NAMESPACE_LIVE_MB")); mb > 0 && t.Name() == "TestManagedNamespaceMountedJstarctlRebind" {
		record := append([]byte("{\"type\":\"event_msg\",\"payload\":{\"message\":\""), bytes.Repeat([]byte("content "), 512)...)
		record = append(record, []byte("\"}}\n")...)
		source = append(source, bytes.Repeat(record, (mb<<20)/len(record))...)
		t.Logf("large folded fixture: %d bytes", len(source))
	}
	input := filepath.Join(root, "input.jsonl")
	if err := os.WriteFile(input, source, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := fold.Fold(context.Background(), fold.Session{ID: "user-thread", RolloutPath: input}, fold.FoldOptions{StoreDir: store, Apply: true, Opaque: true})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifestPath(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: store, ManifestPath: result.ManifestPath, Manifest: manifest, Reader: &namespaceObjectReader{loose: fold.NewObjectStore(store)}, NativeSnapshot: vfs.NativeFile{Path: input, Bytes: result.SourceBytes, SHA256: result.SourceSHA256}})
	if err != nil {
		t.Fatal(err)
	}
	f := NewCanonical()
	f.SetNativeRoot(native)
	if err := f.SetManagedStore(store); err != nil {
		t.Fatal(err)
	}
	if err := f.AddSessionAt("user-thread", route, session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.CloseSessions() })
	return f, store, native, route, source
}

func namespaceRead(t *testing.T, f *Filesystem, name string) []byte {
	t.Helper()
	h, errno := f.Open(name, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open %s: %v", name, errno)
	}
	defer f.Release(h)
	attr, errno := f.Getattr(name)
	if errno != 0 {
		t.Fatal(errno)
	}
	b := make([]byte, attr.Size+100)
	n, errno := f.Read(h, b, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return b[:n]
}

func namespaceWriteTemporary(t *testing.T, f *Filesystem, name string, data []byte) {
	t.Helper()
	h, errno := f.Open(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if errno != 0 {
		t.Fatal(errno)
	}
	if n, errno := f.Write(h, data, 0); errno != 0 || n != len(data) {
		t.Fatalf("temporary write: %d %v", n, errno)
	}
	if errno := f.Fsync(h); errno != 0 {
		t.Fatal(errno)
	}
	if errno := f.Release(h); errno != 0 {
		t.Fatal(errno)
	}
}

func namespacePrepare(t *testing.T, store string) func(context.Context, string, string) (*vfs.Session, io.Closer, error) {
	t.Helper()
	return func(ctx context.Context, source, id string) (*vfs.Session, io.Closer, error) {
		result, err := fold.Fold(ctx, fold.Session{ID: id, RolloutPath: source}, fold.FoldOptions{StoreDir: store, Apply: true, Opaque: true})
		if err != nil {
			return nil, nil, err
		}
		snapshot := filepath.Join(store, "fs", "snapshots", id, "native.jsonl")
		if err := os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
			return nil, nil, err
		}
		input, err := os.Open(source)
		if err != nil {
			return nil, nil, err
		}
		defer input.Close()
		output, err := os.Create(snapshot)
		if err != nil {
			return nil, nil, err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return nil, nil, copyErr
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		manifest, err := fold.LoadManifestPath(result.ManifestPath)
		if err != nil {
			return nil, nil, err
		}
		session, err := vfs.OpenSession(ctx, vfs.SessionOptions{Root: store, ManifestPath: result.ManifestPath, Manifest: manifest, Reader: &namespaceObjectReader{loose: fold.NewObjectStore(store)}, NativeSnapshot: vfs.NativeFile{Path: snapshot, Bytes: result.SourceBytes, SHA256: result.SourceSHA256}})
		return session, nil, err
	}
}

func TestManagedNamespaceRebindBackupReplacementAndReload(t *testing.T) {
	f, store, native, route, original := namespaceFixture(t)
	backup := route + ".20261015_000000.bak"
	temporary := filepath.ToSlash(filepath.Dir(route)) + "/.rollout-user-thread.jsonl.rebind-test"
	rewritten := bytes.ReplaceAll(original, []byte("openai"), []byte("main"))
	old, errno := f.Open(route, os.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer f.Release(old)
	namespaceWriteTemporary(t, f, temporary, rewritten)
	if errno := f.Rename(route, backup); errno != 0 {
		t.Fatalf("backup: %v", errno)
	}
	if _, errno := f.Getattr(route); errno != syscall.ENOENT {
		t.Fatalf("source remained: %v", errno)
	}
	if !bytes.Equal(namespaceRead(t, f, backup), original) {
		t.Fatal("backup changed")
	}
	// A stale SQLite poll must not put the backup back at the original route.
	if err := f.MoveSessionAt("user-thread", route); err != nil {
		t.Fatal(err)
	}
	if _, errno := f.Getattr(route); errno != syscall.ENOENT {
		t.Fatal("stale index resurrected source")
	}
	if errno := f.Rename(temporary, route); errno != 0 {
		t.Fatalf("replace: %v", errno)
	}
	if !bytes.Equal(namespaceRead(t, f, route), rewritten) {
		t.Fatal("new file differs")
	}
	if !bytes.Equal(namespaceRead(t, f, backup), original) {
		t.Fatal("replacement rewrote backup")
	}
	b := make([]byte, len(original)+1)
	if n, errno := f.Read(old, b, 0); errno != 0 || !bytes.Equal(b[:n], original) {
		t.Fatal("old descriptor changed identity")
	}
	if _, errno := f.Getattr(temporary); errno != syscall.ENOENT {
		t.Fatal("temporary remained visible")
	}
	if _, err := os.Stat(nativePathFromRoot(native, backup)); !os.IsNotExist(err) {
		t.Fatal("backup was expanded to a raw file")
	}
	if err := f.FoldNativeReplacements(context.Background(), namespacePrepare(t, store)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(namespaceRead(t, f, route), rewritten) || !bytes.Equal(namespaceRead(t, f, backup), original) {
		t.Fatal("background folding changed either file")
	}
	if _, err := os.Stat(nativePathFromRoot(native, route)); !os.IsNotExist(err) {
		t.Fatal("background cutover left the native current path")
	}
	// Reconstruct solely from durable names and ordinary managed session state.
	reloaded := NewCanonical()
	reloaded.SetNativeRoot(native)
	if err := reloaded.SetManagedStore(store); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reloaded.CloseSessions() })
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := ManagedNamespaceRoutes(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		name := routes[state.SessionID]
		if name == "" {
			continue
		}
		manifest, err := fold.LoadManifestPath(state.ManifestPath)
		if err != nil {
			t.Fatal(err)
		}
		session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: store, ManifestPath: state.ManifestPath, Manifest: manifest, Reader: &namespaceObjectReader{loose: fold.NewObjectStore(store)}, NativeSnapshot: state.NativeSnapshot})
		if err != nil {
			t.Fatal(err)
		}
		if err := reloaded.UpsertSessionAt(state.SessionID, name, session); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(namespaceRead(t, reloaded, route), rewritten) || !bytes.Equal(namespaceRead(t, reloaded, backup), original) {
		t.Fatal("durable rename did not survive reload")
	}
	// Rollback is also a normal rename, not deletion of either source tree.
	if errno := reloaded.Rename(backup, route); errno != 0 {
		t.Fatalf("restore: %v", errno)
	}
	if !bytes.Equal(namespaceRead(t, reloaded, route), original) {
		t.Fatal("backup restore failed")
	}
}

func TestManagedNamespaceDirectReplacementPreservesOpenTarget(t *testing.T) {
	f, store, _, route, original := namespaceFixture(t)
	old, errno := f.Open(route, os.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer f.Release(old)
	temporary := filepath.ToSlash(filepath.Dir(route)) + "/.replace.tmp"
	want := []byte("{\"new\":true}\n")
	namespaceWriteTemporary(t, f, temporary, want)
	if errno := f.Rename(temporary, route); errno != 0 {
		t.Fatal(errno)
	}
	if !bytes.Equal(namespaceRead(t, f, route), want) {
		t.Fatal("replacement missing")
	}
	b := make([]byte, len(original)+1)
	if n, errno := f.Read(old, b, 0); errno != 0 || !bytes.Equal(b[:n], original) {
		t.Fatal("replaced open descriptor changed")
	}
	if err := f.RetireReplacedFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := vfs.LoadSessionDeletion(store, "user-thread"); err != nil {
		t.Fatalf("overwritten inode was not scheduled for reclamation: %v", err)
	}
	if n, errno := f.Read(old, b, 0); errno != 0 || !bytes.Equal(b[:n], original) {
		t.Fatal("retirement broke old descriptor")
	}
	if !bytes.Equal(namespaceRead(t, f, route), want) {
		t.Fatal("retirement hid replacement")
	}
}

func TestManagedNamespaceBackupCanBeDeletedWithoutDeletingReplacement(t *testing.T) {
	f, store, _, route, original := namespaceFixture(t)
	f.SetSessionDeletionPublisher(func(state vfs.SessionState, name string) error {
		_, err := vfs.PublishSessionDeletion(store, state, name)
		return err
	})
	backup := route + ".bak"
	if errno := f.Rename(route, backup); errno != 0 {
		t.Fatal(errno)
	}
	temporary := filepath.ToSlash(filepath.Dir(route)) + "/.rebind-test"
	want := bytes.ReplaceAll(original, []byte("openai"), []byte("main"))
	namespaceWriteTemporary(t, f, temporary, want)
	if errno := f.Rename(temporary, route); errno != 0 {
		t.Fatal(errno)
	}
	if errno := f.Unlink(backup); errno != 0 {
		t.Fatalf("delete backup: %v", errno)
	}
	if !bytes.Equal(namespaceRead(t, f, route), want) {
		t.Fatal("backup deletion removed current file")
	}
}

func TestManagedNamespaceFailedReplacementRestoresBackup(t *testing.T) {
	f, _, _, route, original := namespaceFixture(t)
	backup := route + ".bak"
	if errno := f.Rename(route, backup); errno != 0 {
		t.Fatal(errno)
	}
	if errno := f.Rename(filepath.ToSlash(filepath.Dir(route))+"/.missing-rebind", route); errno != syscall.ENOENT {
		t.Fatalf("missing source: %v", errno)
	}
	if errno := f.Rename(backup, route); errno != 0 {
		t.Fatal(errno)
	}
	if !bytes.Equal(namespaceRead(t, f, route), original) {
		t.Fatal("failed replacement lost history")
	}
}

func namespaceNativeReplacement(t *testing.T, f *Filesystem, route string, content []byte) {
	t.Helper()
	temporary := filepath.ToSlash(filepath.Dir(route)) + "/.replacement"
	namespaceWriteTemporary(t, f, temporary, content)
	if errno := f.Rename(temporary, route); errno != 0 {
		t.Fatal(errno)
	}
}

func TestManagedNamespaceRestoreBeforeBackgroundFold(t *testing.T) {
	f, store, native, route, original := namespaceFixture(t)
	backup := route + ".bak"
	if errno := f.Rename(route, backup); errno != 0 {
		t.Fatal(errno)
	}
	namespaceNativeReplacement(t, f, route, []byte("{\"replacement\":true}\n"))
	if errno := f.Rename(backup, route); errno != 0 {
		t.Fatal(errno)
	}
	if err := f.FoldNativeReplacements(context.Background(), func(context.Context, string, string) (*vfs.Session, io.Closer, error) {
		t.Fatal("restored file was queued for native folding")
		return nil, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(namespaceRead(t, f, route), original) {
		t.Fatal("restored backup changed")
	}
	if _, err := os.Stat(nativePathFromRoot(native, route)); !os.IsNotExist(err) {
		t.Fatal("replaced native bytes leaked")
	}
	n, err := readManagedNamespace(store)
	if err != nil || len(n.Native) != 0 {
		t.Fatalf("stale native authority after restore: %+v %v", n, err)
	}
}

func TestManagedNamespaceBackgroundFoldSkipsOpenNativeHandle(t *testing.T) {
	f, store, _, route, _ := namespaceFixture(t)
	want := []byte("{\"replacement\":true}\n")
	namespaceNativeReplacement(t, f, route, want)
	handle, errno := f.Open(route, os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatal(errno)
	}
	if err := f.FoldNativeReplacements(context.Background(), func(context.Context, string, string) (*vfs.Session, io.Closer, error) {
		t.Fatal("attempted compression while native descriptor was open")
		return nil, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if errno := f.Release(handle); errno != 0 {
		t.Fatal(errno)
	}
	if err := f.FoldNativeReplacements(context.Background(), namespacePrepare(t, store)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(namespaceRead(t, f, route), want) {
		t.Fatal("closed native handle did not fold correctly")
	}
}

func TestManagedNamespaceConcurrentChangesDoNotCommitStaleFold(t *testing.T) {
	for _, action := range []string{"append", "unlink", "rename", "restore"} {
		t.Run(action, func(t *testing.T) {
			f, store, _, route, original := namespaceFixture(t)
			backup := route + ".bak"
			if errno := f.Rename(route, backup); errno != 0 {
				t.Fatal(errno)
			}
			content := []byte("{\"replacement\":true}\n")
			namespaceNativeReplacement(t, f, route, content)
			prepare := namespacePrepare(t, store)
			err := f.FoldNativeReplacements(context.Background(), func(ctx context.Context, source, id string) (*vfs.Session, io.Closer, error) {
				session, closer, err := prepare(ctx, source, id)
				if err != nil {
					return session, closer, err
				}
				switch action {
				case "append":
					h, errno := f.Open(route, os.O_WRONLY|os.O_APPEND)
					if errno != 0 {
						t.Fatal(errno)
					}
					tail := []byte("{\"tail\":true}\n")
					if _, errno := f.Write(h, tail, int64(len(content))); errno != 0 {
						t.Fatal(errno)
					}
					if errno := f.Release(h); errno != 0 {
						t.Fatal(errno)
					}
					content = append(content, tail...)
				case "unlink":
					if errno := f.Unlink(route); errno != 0 {
						t.Fatal(errno)
					}
				case "rename":
					if errno := f.Rename(route, route+".moved"); errno != 0 {
						t.Fatal(errno)
					}
				case "restore":
					if errno := f.Rename(backup, route); errno != 0 {
						t.Fatal(errno)
					}
				}
				return session, closer, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "append":
				if !bytes.Equal(namespaceRead(t, f, route), content) {
					t.Fatal("lost concurrent append")
				}
			case "unlink", "rename":
				if _, errno := f.Getattr(route); errno != syscall.ENOENT {
					t.Fatal("stale fold resurrected source")
				}
				if action == "rename" && !bytes.Equal(namespaceRead(t, f, route+".moved"), content) {
					t.Fatal("lost renamed replacement")
				}
			case "restore":
				if !bytes.Equal(namespaceRead(t, f, route), original) {
					t.Fatal("stale fold overwrote restored backup")
				}
			}
		})
	}
}

func TestManagedNamespaceNativeRenameIntentReplay(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			f, store, native, route, original := namespaceFixture(t)
			temporary := filepath.ToSlash(filepath.Dir(route)) + "/.rebind-interrupted"
			content := []byte("{\"new\":true}\n")
			namespaceWriteTemporary(t, f, temporary, content)
			info, err := os.Stat(nativePathFromRoot(native, temporary))
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			n := f.cloneManagedNamespaceLocked()
			n.Move = &nativeRenameIntent{Source: temporary, Target: route, ObjectID: fileObjectIdentity(info), Displaced: "user-thread"}
			err = f.publishManagedNamespaceLocked(n)
			f.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if committed {
				if err := os.Rename(nativePathFromRoot(native, temporary), nativePathFromRoot(native, route)); err != nil {
					t.Fatal(err)
				}
			}
			reloaded := NewCanonical()
			reloaded.SetNativeRoot(native)
			if err := reloaded.SetManagedStore(store); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reloaded.CloseSessions() })
			if err := reloaded.AddSessionAt("user-thread", route, f.sessions["user-thread"]); err != nil {
				t.Fatal(err)
			}
			want := original
			if committed {
				want = content
			}
			if !bytes.Equal(namespaceRead(t, reloaded, route), want) {
				t.Fatal("intent replay chose wrong inode")
			}
		})
	}
}
