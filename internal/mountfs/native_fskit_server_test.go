package mountfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/fskitproto"
	"github.com/jstar0/codexfold/internal/mountid"
)

func TestNativeFSKitServerPreservesJSONLWritesAndNamespaceMutations(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	for _, directory := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(nativeRoot, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	defer stop()
	defer client.Close()

	for _, directory := range []string{"/sessions/2026", "/sessions/2026/07", "/sessions/2026/07/17"} {
		encoder := fskitproto.NewEncoder(128)
		encoder.String(directory)
		encoder.Uint32(0o700)
		if _, err := client.Call(fskitproto.OpMkdir, encoder.Data()); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}

	filePath := "/sessions/2026/07/17/session.jsonl"
	create := fskitproto.NewEncoder(128)
	create.String(filePath)
	create.Uint32(uint32(os.O_RDWR | os.O_APPEND))
	created, err := client.Call(fskitproto.OpCreate, create.Data())
	if err != nil {
		t.Fatal(err)
	}
	createdDecoder := fskitproto.NewDecoder(created)
	handle, err := createdDecoder.Uint64()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createdDecoder.Entry(); err != nil {
		t.Fatal(err)
	}
	if err := createdDecoder.Done(); err != nil {
		t.Fatal(err)
	}

	first := []byte("{\"record\":1}\n")
	second := []byte("{\"record\":2}\n")
	writeNativeFSKitTestPayload(t, client, handle, 0, first)
	writeNativeFSKitTestPayload(t, client, handle, int64(len(first)), second)
	callNativeFSKitHandle(t, client, fskitproto.OpFsync, handle)

	read := fskitproto.NewEncoder(24)
	read.Uint64(handle)
	read.Int64(0)
	read.Uint32(4096)
	readPayload, err := client.Call(fskitproto.OpRead, read.Data())
	if err != nil {
		t.Fatal(err)
	}
	readDecoder := fskitproto.NewDecoder(readPayload)
	got, err := readDecoder.Bytes(4096)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), first...), second...)
	if !bytes.Equal(got, want) {
		t.Fatalf("visible bytes = %q, want %q", got, want)
	}
	callNativeFSKitHandle(t, client, fskitproto.OpRelease, handle)

	for _, directory := range []string{"/archived_sessions/2026", "/archived_sessions/2026/07", "/archived_sessions/2026/07/17"} {
		encoder := fskitproto.NewEncoder(128)
		encoder.String(directory)
		encoder.Uint32(0o700)
		if _, err := client.Call(fskitproto.OpMkdir, encoder.Data()); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}
	archivedPath := "/archived_sessions/2026/07/17/session.jsonl"
	rename := fskitproto.NewEncoder(256)
	rename.String(filePath)
	rename.String(archivedPath)
	if _, err := client.Call(fskitproto.OpRename, rename.Data()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(nativeRoot, "archived_sessions/2026/07/17/session.jsonl")); err != nil {
		t.Fatal(err)
	}

	unlink := fskitproto.NewEncoder(128)
	unlink.String(archivedPath)
	if _, err := client.Call(fskitproto.OpUnlink, unlink.Data()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(nativeRoot, "archived_sessions/2026/07/17/session.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archived path still exists: %v", err)
	}
}

func TestNativeFSKitServerRejectsWrongToken(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	defer stop()

	resource, err := os.ReadFile(filepath.Join(root, "resource.bin"))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := fskitproto.DecodeDescriptor(resource)
	if err != nil {
		t.Fatal(err)
	}
	descriptor.Token[0] ^= 0xff
	if _, err := fskitproto.Dial(descriptor, time.Second); fskitproto.ErrorNumber(err) != syscall.EACCES {
		t.Fatalf("wrong token error = %v, want EACCES", err)
	}
}

func TestNativeFSKitServerExposesReadOnlyMountIdentity(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	defer stop()
	defer client.Close()

	readdir := fskitproto.NewEncoder(16)
	readdir.String("/")
	response, err := client.Call(fskitproto.OpReadDir, readdir.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder := fskitproto.NewDecoder(response)
	count, err := decoder.Uint32()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for range count {
		entry, err := decoder.Entry()
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name == mountid.Path {
			found = true
		}
	}
	if err := decoder.Done(); err != nil || !found {
		t.Fatalf("mount identity listed=%t err=%v", found, err)
	}

	open := fskitproto.NewEncoder(128)
	open.String("/" + mountid.Path)
	open.Uint32(uint32(os.O_RDONLY))
	response, err = client.Call(fskitproto.OpOpen, open.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder = fskitproto.NewDecoder(response)
	handle, err := decoder.Uint64()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("identity open handle=%d err=%v", handle, err)
	}
	read := fskitproto.NewEncoder(24)
	read.Uint64(handle)
	read.Int64(0)
	read.Uint32(4096)
	response, err = client.Call(fskitproto.OpRead, read.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder = fskitproto.NewDecoder(response)
	identityBytes, err := decoder.Bytes(4096)
	if err != nil || decoder.Done() != nil {
		t.Fatalf("decode identity: %v", err)
	}
	identity, err := mountid.Parse(identityBytes)
	if err != nil || identity.BuildSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("mount identity = %#v err=%v", identity, err)
	}
	callNativeFSKitHandle(t, client, fskitproto.OpRelease, handle)

	writeOpen := fskitproto.NewEncoder(128)
	writeOpen.String("/" + mountid.Path)
	writeOpen.Uint32(uint32(os.O_WRONLY))
	if _, err := client.Call(fskitproto.OpOpen, writeOpen.Data()); fskitproto.ErrorNumber(err) != syscall.EPERM {
		t.Fatalf("write identity open error = %v, want EPERM", err)
	}
}

func TestNativeFSKitServerPublishesDirectoryResourceWithScopedSocket(t *testing.T) {
	root := shortNativeFSKitTestDir(t, "cfs-r-")
	resource := filepath.Join(root, "native-fskit")
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	options := NativeFSKitServerOptions{
		SocketPath: filepath.Join(resource, "daemon.sock"), ResourcePath: resource,
		Token: bytes.Repeat([]byte{0x24}, 32), Generation: 91, BuildSHA256: strings.Repeat("b", 64),
	}
	go func() { done <- ServeNativeFSKit(ctx, filesystem, options) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("directory resource server shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("directory resource server did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var client *fskitproto.Client
	var err error
	for time.Now().Before(deadline) {
		client, err = fskitproto.DialResource(resource, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial directory resource: %v", err)
	}
	defer client.Close()
	if _, err := os.Stat(filepath.Join(resource, fskitproto.DescriptorFilename)); err != nil {
		t.Fatalf("descriptor file: %v", err)
	}
	if _, err := os.Lstat(options.SocketPath); err != nil {
		t.Fatalf("scoped socket: %v", err)
	}
}

func TestNativeFSKitNodesNeverReuseObjectIDsAcrossNamespaceRefresh(t *testing.T) {
	nodes := nativeFSKitNodes{next: 3, byPath: map[string]uint64{"/": 2}}
	first := nodes.node("/sessions/first")
	nodes.syncVersion(2)
	second := nodes.node("/sessions/second")
	if second == first {
		t.Fatalf("object ID %d was reused after namespace refresh", second)
	}
	if second <= first {
		t.Fatalf("object IDs did not remain monotonic: first=%d second=%d", first, second)
	}
}

func TestNativeFSKitServerNormalizesFSKitWholeFileSnapshotsIntoJSONLAppends(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	targetDirectory := filepath.Join(nativeRoot, "sessions", "2026", "07", "17")
	if err := os.MkdirAll(targetDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(nativeRoot, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":0}\n")
	target := filepath.Join(targetDirectory, "session.jsonl")
	if err := os.WriteFile(target, base, 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	defer stop()
	defer client.Close()

	open := fskitproto.NewEncoder(128)
	open.String("/sessions/2026/07/17/session.jsonl")
	open.Uint32(uint32(os.O_RDWR|os.O_APPEND) | fskitproto.OpenFlagSnapshot)
	response, err := client.Call(fskitproto.OpOpen, open.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder := fskitproto.NewDecoder(response)
	handle, err := decoder.Uint64()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("open handle=%d err=%v", handle, err)
	}
	first := append(append([]byte(nil), base...), []byte("{\"record\":1}\n")...)
	second := append(append([]byte(nil), base...), []byte("{\"record\":2}\n")...)
	writeNativeFSKitTestPayload(t, client, handle, 0, first)
	writeNativeFSKitTestPayload(t, client, handle, 0, second)
	callNativeFSKitHandle(t, client, fskitproto.OpFsync, handle)
	callNativeFSKitHandle(t, client, fskitproto.OpRelease, handle)

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), base...), []byte("{\"record\":1}\n")...), []byte("{\"record\":2}\n")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("normalized bytes = %q, want %q", got, want)
	}

	reopen := fskitproto.NewEncoder(128)
	reopen.String("/sessions/2026/07/17/session.jsonl")
	reopen.Uint32(uint32(os.O_RDWR|os.O_APPEND) | fskitproto.OpenFlagSnapshot)
	response, err = client.Call(fskitproto.OpOpen, reopen.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder = fskitproto.NewDecoder(response)
	handle, err = decoder.Uint64()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("reopen handle=%d err=%v", handle, err)
	}
	replacement := bytes.Replace(want, []byte("{\"record\":0}"), []byte("{\"record\":9}"), 1)
	writeNativeFSKitTestPayload(t, client, handle, 0, replacement)
	callNativeFSKitHandle(t, client, fskitproto.OpFsync, handle)
	callNativeFSKitHandle(t, client, fskitproto.OpRelease, handle)
	got, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("random snapshot bytes = %q, want %q", got, replacement)
	}
}

func startNativeFSKitTestServer(t *testing.T, filesystem *Filesystem, root string) (*fskitproto.Client, func()) {
	t.Helper()
	socketRoot := shortNativeFSKitTestDir(t, "cfs-")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	options := NativeFSKitServerOptions{
		SocketPath: filepath.Join(socketRoot, "daemon.sock"), ResourcePath: filepath.Join(root, "resource.bin"),
		Token: bytes.Repeat([]byte{0x42}, 32), Generation: 77, BuildSHA256: strings.Repeat("a", 64),
	}
	go func() { done <- ServeNativeFSKit(ctx, filesystem, options) }()
	deadline := time.Now().Add(5 * time.Second)
	var client *fskitproto.Client
	var dialErr error
	for time.Now().Before(deadline) {
		select {
		case serveErr := <-done:
			cancel()
			t.Fatalf("FSKit test server exited during startup: %v", serveErr)
		default:
		}
		client, dialErr = fskitproto.DialResource(options.ResourcePath, 100*time.Millisecond)
		if dialErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if dialErr != nil {
		cancel()
		t.Fatalf("start FSKit test server: %v", dialErr)
	}
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("FSKit server shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("FSKit server did not stop")
		}
	}
	return client, stop
}

func shortNativeFSKitTestDir(t *testing.T, pattern string) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func writeNativeFSKitTestPayload(t *testing.T, client *fskitproto.Client, handle uint64, offset int64, data []byte) {
	t.Helper()
	encoder := fskitproto.NewEncoder(20 + len(data))
	encoder.Uint64(handle)
	encoder.Int64(offset)
	encoder.Bytes(data)
	response, err := client.Call(fskitproto.OpWrite, encoder.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder := fskitproto.NewDecoder(response)
	written, err := decoder.Uint32()
	if err != nil || decoder.Done() != nil || int(written) != len(data) {
		t.Fatalf("write response bytes=%d err=%v", written, err)
	}
}

func callNativeFSKitHandle(t *testing.T, client *fskitproto.Client, operation fskitproto.Op, handle uint64) {
	t.Helper()
	encoder := fskitproto.NewEncoder(8)
	encoder.Uint64(handle)
	if _, err := client.Call(operation, encoder.Data()); err != nil {
		t.Fatal(err)
	}
}
