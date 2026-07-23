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

	"github.com/samekind/codexfold/internal/fskitproto"
	"github.com/samekind/codexfold/internal/mountid"
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

func TestNativeFSKitServerNamespaceRefreshGuardCannotCreateMissingPaths(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	parent := filepath.Join(nativeRoot, "sessions", "2099", "12", "31")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(nativeRoot, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	defer stop()
	defer client.Close()

	filePath := "/sessions/2099/12/31/raced.jsonl"
	releaseFile := filesystem.beginNativeNamespaceRefresh(filePath)
	create := fskitproto.NewEncoder(128)
	create.String(filePath)
	create.Uint32(uint32(os.O_RDONLY))
	if _, err := client.Call(fskitproto.OpCreate, create.Data()); fskitproto.ErrorNumber(err) != syscall.ENOENT {
		t.Fatalf("guarded missing create error = %v, want ENOENT", err)
	}
	releaseFile()
	if _, err := os.Lstat(filepath.Join(parent, "raced.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guarded refresh created a missing file: %v", err)
	}

	directoryPath := "/sessions/2099/12/31/raced-directory"
	releaseDirectory := filesystem.beginNativeNamespaceRefresh(directoryPath)
	mkdir := fskitproto.NewEncoder(128)
	mkdir.String(directoryPath)
	mkdir.Uint32(0o700)
	if _, err := client.Call(fskitproto.OpMkdir, mkdir.Data()); fskitproto.ErrorNumber(err) != syscall.ENOENT {
		t.Fatalf("guarded missing mkdir error = %v, want ENOENT", err)
	}
	releaseDirectory()
	if _, err := os.Lstat(filepath.Join(parent, "raced-directory")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guarded refresh created a missing directory: %v", err)
	}

	created, err := client.Call(fskitproto.OpCreate, create.Data())
	if err != nil {
		t.Fatalf("ordinary create after guard: %v", err)
	}
	decoder := fskitproto.NewDecoder(created)
	handle, err := decoder.Uint64()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Entry(); err != nil || decoder.Done() != nil {
		t.Fatalf("decode ordinary create after guard: %v", err)
	}
	callNativeFSKitHandle(t, client, fskitproto.OpRelease, handle)
	if _, err := client.Call(fskitproto.OpMkdir, mkdir.Data()); err != nil {
		t.Fatalf("ordinary mkdir after guard: %v", err)
	}
}

func TestNativeFSKitServerNamespaceRefreshLeavesExistingFileUnchanged(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	parent := filepath.Join(nativeRoot, "sessions", "2099", "12", "31")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(nativeRoot, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "external.jsonl")
	want := []byte("external-content\n")
	if err := os.WriteFile(target, want, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	defer stop()
	defer client.Close()

	filePath := "/sessions/2099/12/31/external.jsonl"
	release := filesystem.beginNativeNamespaceRefresh(filePath)
	create := fskitproto.NewEncoder(128)
	create.String(filePath)
	create.Uint32(uint32(os.O_RDONLY))
	if _, err := client.Call(fskitproto.OpCreate, create.Data()); fskitproto.ErrorNumber(err) != syscall.EEXIST {
		release()
		t.Fatalf("guarded existing create error = %v, want EEXIST", err)
	}
	release()
	after, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || after.Mode() != before.Mode() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("refresh changed native file: bytes=%q mode=%v size=%d mtime=%s", got, after.Mode(), after.Size(), after.ModTime())
	}
}

func TestNativeFSKitServerStreamsLargeVirtualReadAsOneResponse(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	line := []byte("{\"stream\":\"bounded\"}\n")
	source := bytes.Repeat(line, (3<<20)/len(line)+1)
	virtualPath := "/archived_sessions/large-virtual.jsonl"
	if err := filesystem.AddSessionAt("large-virtual", virtualPath, mountSessionFixture(t, "large-virtual", source)); err != nil {
		t.Fatal(err)
	}
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	defer stop()
	defer client.Close()

	open := fskitproto.NewEncoder(128)
	open.String(virtualPath)
	open.Uint32(uint32(os.O_RDONLY))
	response, err := client.Call(fskitproto.OpOpen, open.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder := fskitproto.NewDecoder(response)
	handle, err := decoder.Uint64()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("open handle=%d err=%v", handle, err)
	}
	defer callNativeFSKitHandle(t, client, fskitproto.OpRelease, handle)

	read := fskitproto.NewEncoder(24)
	read.Uint64(handle)
	read.Int64(0)
	read.Uint32(uint32(len(source)))
	response, err = client.Call(fskitproto.OpRead, read.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder = fskitproto.NewDecoder(response)
	got, err := decoder.Bytes(len(source))
	if err != nil || decoder.Done() != nil || !bytes.Equal(got, source) {
		t.Fatalf("large virtual response bytes=%d err=%v", len(got), err)
	}

	read = fskitproto.NewEncoder(24)
	read.Uint64(handle)
	read.Int64(int64(len(source) - 13))
	read.Uint32(4096)
	response, err = client.Call(fskitproto.OpRead, read.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder = fskitproto.NewDecoder(response)
	got, err = decoder.Bytes(4096)
	if err != nil || decoder.Done() != nil || !bytes.Equal(got, source[len(source)-13:]) {
		t.Fatalf("large virtual EOF response bytes=%d err=%v", len(got), err)
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

func TestNativeFSKitNodesKeepUnchangedObjectsAndNeverReuseReplacements(t *testing.T) {
	nodes := nativeFSKitNodes{next: 3, byPath: map[string]nativeFSKitNode{"/": {id: 2}}}
	first := nodes.node("/sessions/first", "native:1:10")
	nodes.syncVersion(2)
	if unchanged := nodes.node("/sessions/first", "native:1:10"); unchanged != first {
		t.Fatalf("unchanged object ID = %d, want %d", unchanged, first)
	}
	replaced := nodes.node("/sessions/first", "native:1:11")
	if replaced == first || replaced <= first {
		t.Fatalf("replacement object ID = %d, want a fresh ID after %d", replaced, first)
	}
	nodes.forget("/sessions/first")
	recreated := nodes.node("/sessions/first", "native:1:11")
	if recreated == replaced || recreated <= replaced {
		t.Fatalf("recreated object ID = %d, want a fresh ID after %d", recreated, replaced)
	}
}

func TestNativeFSKitHelloNegotiatesContentGenerationWithoutBreakingLegacyPeers(t *testing.T) {
	token := []byte("0123456789abcdef")
	connection := &nativeFSKitConnection{server: &nativeFSKitServer{
		filesystem: NewCanonical(), token: token, maxPayload: fskitproto.DefaultMaxPayload,
	}}

	legacyRequest := fskitproto.NewEncoder(32)
	legacyRequest.Bytes(token)
	legacyResponse, status := connection.hello(legacyRequest.Data())
	if status != 0 {
		t.Fatalf("legacy hello status = %d", status)
	}
	legacyDecoder := fskitproto.NewDecoder(legacyResponse)
	if _, err := legacyDecoder.Uint32(); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDecoder.Uint64(); err != nil || legacyDecoder.Done() != nil {
		t.Fatalf("legacy hello response changed shape: %v", err)
	}

	negotiatedRequest := fskitproto.NewEncoder(36)
	negotiatedRequest.Bytes(token)
	negotiatedRequest.Uint32(fskitproto.CapabilityContentGeneration)
	negotiatedResponse, status := connection.hello(negotiatedRequest.Data())
	if status != 0 {
		t.Fatalf("negotiated hello status = %d", status)
	}
	negotiatedDecoder := fskitproto.NewDecoder(negotiatedResponse)
	if _, err := negotiatedDecoder.Uint32(); err != nil {
		t.Fatal(err)
	}
	if _, err := negotiatedDecoder.Uint64(); err != nil {
		t.Fatal(err)
	}
	accepted, err := negotiatedDecoder.Uint32()
	if err != nil || negotiatedDecoder.Done() != nil {
		t.Fatalf("negotiated hello response decode: accepted=%#x err=%v", accepted, err)
	}
	if accepted != fskitproto.CapabilityContentGeneration {
		t.Fatalf("accepted capabilities = %#x, want %#x", accepted, fskitproto.CapabilityContentGeneration)
	}
}

func TestNativeFSKitServerObjectIDsFollowNativeIdentityNotNamespaceVersion(t *testing.T) {
	root := t.TempDir()
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(root, namespace), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "sessions", "target.jsonl")
	if err := os.WriteFile(target, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	server := &nativeFSKitServer{
		filesystem: filesystem,
		nodes: nativeFSKitNodes{
			version: filesystem.NamespaceVersion(),
			next:    4,
			byPath:  map[string]nativeFSKitNode{"/": {id: 2}},
		},
	}
	first, errno := server.entry("/sessions/target.jsonl")
	if errno != 0 {
		t.Fatalf("first entry: %v", errno)
	}
	parentBefore, errno := server.entry("/sessions")
	if errno != 0 {
		t.Fatalf("parent before generation change: %v", errno)
	}
	filesystem.bumpDirectoryGeneration("/sessions")
	filesystem.bumpNamespaceVersion()
	server.nodes.syncVersion(filesystem.NamespaceVersion())
	parentChanged, errno := server.entry("/sessions/target.jsonl")
	if errno != 0 {
		t.Fatalf("entry after parent generation: %v", errno)
	}
	if parentChanged.NodeID != first.NodeID {
		t.Fatalf("parent generation replaced child object ID %d with %d", first.NodeID, parentChanged.NodeID)
	}
	if parentChanged.ParentID != first.ParentID {
		t.Fatalf("parent generation replaced stable parent ID %d with %d", first.ParentID, parentChanged.ParentID)
	}
	parentAfter, errno := server.entry("/sessions")
	if errno != 0 {
		t.Fatalf("parent after generation change: %v", errno)
	}
	if parentAfter.NodeID != parentBefore.NodeID {
		t.Fatalf("parent generation replaced parent node ID %d with %d", parentBefore.NodeID, parentAfter.NodeID)
	}
	if parentAfter.ContentGeneration <= parentBefore.ContentGeneration {
		t.Fatalf("parent content generation did not advance: before=%d after=%d", parentBefore.ContentGeneration, parentAfter.ContentGeneration)
	}
	first = parentChanged

	if err := os.WriteFile(filepath.Join(root, "archived_sessions", "unrelated.jsonl"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem.bumpNamespaceVersion()
	server.nodes.syncVersion(filesystem.NamespaceVersion())
	unchanged, errno := server.entry("/sessions/target.jsonl")
	if errno != 0 {
		t.Fatalf("unchanged entry: %v", errno)
	}
	if unchanged.NodeID != first.NodeID {
		t.Fatalf("unrelated namespace change replaced object ID %d with %d", first.NodeID, unchanged.NodeID)
	}

	oldFile, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem.bumpNamespaceVersion()
	server.nodes.syncVersion(filesystem.NamespaceVersion())
	replaced, errno := server.entry("/sessions/target.jsonl")
	if errno != 0 {
		t.Fatalf("replacement entry: %v", errno)
	}
	if replaced.NodeID == first.NodeID || replaced.NodeID <= first.NodeID {
		t.Fatalf("replacement object ID = %d, want a fresh ID after %d", replaced.NodeID, first.NodeID)
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
