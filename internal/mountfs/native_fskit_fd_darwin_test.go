//go:build darwin

package mountfs

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/fskitproto"
	"golang.org/x/sys/unix"
)

func TestNativeFSKitServerTransfersNativeReadFDOnlyForReadOnlyCapability(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	nativePath := filepath.Join(nativeRoot, "sessions", "2026", "07", "17", "performance.bin")
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("codexfold-native-fd\n"), 4096)
	if err := os.WriteFile(nativePath, want, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	_ = client.Close()
	defer stop()

	descriptorData, err := os.ReadFile(filepath.Join(root, "resource.bin"))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := fskitproto.DecodeDescriptor(descriptorData)
	if err != nil {
		t.Fatal(err)
	}
	connection := dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()

	callNativeFDTestHello(t, connection, descriptor, true)
	openPayload := fskitproto.NewEncoder(128)
	openPayload.String("/sessions/2026/07/17/performance.bin")
	openPayload.Uint32(uint32(os.O_RDONLY))
	response := callNativeFDTestFrame(t, connection, descriptor, 2, fskitproto.OpOpen, openPayload.Data())
	if response.Flags&fskitproto.FlagNativeReadFD == 0 {
		t.Fatalf("read-only open flags = %#x, want native FD", response.Flags)
	}
	decoder := fskitproto.NewDecoder(response.Payload)
	handle, err := decoder.Uint64()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("decode native open handle=%d err=%v", handle, err)
	}

	fd := receiveNativeFDTestMarker(t, connection)
	file := os.NewFile(uintptr(fd), "codexfold-native-fd-test")
	if file == nil {
		t.Fatalf("construct received file for fd %d", fd)
	}
	got, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read received FD: read=%v close=%v", readErr, closeErr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("received FD bytes differ: got=%d want=%d", len(got), len(want))
	}

	callNativeFDTestHandle(t, connection, descriptor, 3, fskitproto.OpRelease, handle)

	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	connection = dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()
	callNativeFDTestHello(t, connection, descriptor, true)
	openPayload = fskitproto.NewEncoder(128)
	openPayload.String("/sessions/2026/07/17/performance.bin")
	openPayload.Uint32(uint32(os.O_RDWR))
	response = callNativeFDTestFrame(t, connection, descriptor, 2, fskitproto.OpOpen, openPayload.Data())
	if response.Flags&fskitproto.FlagNativeReadFD != 0 {
		t.Fatalf("writable open unexpectedly transferred native FD: flags=%#x", response.Flags)
	}
}

func TestConfigureNativeFSKitSocketRaisesStreamBuffers(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "codexfold-buffer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	address := &net.UnixAddr{Name: filepath.Join(root, "buffer.sock"), Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan *net.UnixConn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			acceptErrors <- acceptErr
			return
		}
		accepted <- connection
	}()
	client, err := net.DialUnix("unix", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var server *net.UnixConn
	select {
	case err := <-acceptErrors:
		t.Fatal(err)
	case server = <-accepted:
	}
	defer server.Close()
	if err := configureNativeFSKitSocket(server); err != nil {
		t.Fatal(err)
	}
	for _, option := range []int{syscall.SO_RCVBUF, syscall.SO_SNDBUF} {
		value := unixSocketOption(t, server, option)
		if value < nativeFSKitSocketBufferBytes {
			t.Fatalf("socket option %d = %d, want at least %d", option, value, nativeFSKitSocketBufferBytes)
		}
	}
}

func TestNativeFSKitServerTransfersSharedReadFDForLargeVirtualReads(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	line := []byte("{\"shared_read_fd\":true}\n")
	source := bytes.Repeat(line, (6<<20)/len(line)+1)
	virtualPath := "/archived_sessions/shared-read.jsonl"
	if err := filesystem.AddSessionAt("shared-read", virtualPath, mountSessionFixture(t, "shared-read", source)); err != nil {
		t.Fatal(err)
	}
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	_ = client.Close()
	defer stop()
	descriptor := readNativeFDTestDescriptor(t, root)
	connection := dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()
	callNativeFDTestHelloCapabilities(t, connection, descriptor, fskitproto.CapabilitySharedReadFD)

	handle := openNativeFDTestPath(t, connection, descriptor, 2, virtualPath)
	readLength := 5 << 20
	response := callNativeFDTestRead(t, connection, descriptor, 3, handle, 0, readLength)
	if response.Flags&fskitproto.FlagSharedReadFD == 0 {
		t.Fatalf("large virtual read flags = %#x, want shared FD", response.Flags)
	}
	count := decodeNativeFDTestReadCount(t, response)
	if count != readLength {
		t.Fatalf("large virtual read count = %d, want %d", count, readLength)
	}
	fd := receiveFDTestMarker(t, connection, fskitproto.SharedReadFDMarker)
	assertMappedNativeFDTestBytes(t, fd, source[:readLength])
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}

	offset := int64(len(source) - 97)
	response = callNativeFDTestRead(t, connection, descriptor, 4, handle, offset, nativeFSKitSharedReadMinimumBytes)
	if response.Flags&fskitproto.FlagSharedReadFD == 0 {
		t.Fatalf("EOF virtual read flags = %#x, want shared FD", response.Flags)
	}
	count = decodeNativeFDTestReadCount(t, response)
	if count != 97 {
		t.Fatalf("EOF virtual read count = %d, want 97", count)
	}
	fd = receiveFDTestMarker(t, connection, fskitproto.SharedReadFDMarker)
	assertMappedNativeFDTestBytes(t, fd, source[len(source)-97:], nativeFSKitSharedReadMinimumBytes)
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}
	callNativeFDTestHandle(t, connection, descriptor, 5, fskitproto.OpRelease, handle)
}

func TestNativeFSKitServerReusesSharedWindowAcrossVirtualReads(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	line := []byte("{\"shared_window\":true}\n")
	source := bytes.Repeat(line, (7<<20)/len(line)+1)
	virtualPath := "/archived_sessions/shared-window.jsonl"
	if err := filesystem.AddSessionAt("shared-window", virtualPath, mountSessionFixture(t, "shared-window", source)); err != nil {
		t.Fatal(err)
	}
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	_ = client.Close()
	defer stop()
	descriptor := readNativeFDTestDescriptor(t, root)
	connection := dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()
	callNativeFDTestHelloCapabilities(t, connection, descriptor, fskitproto.CapabilitySharedWindow)

	openPayload := fskitproto.NewEncoder(128)
	openPayload.String(virtualPath)
	openPayload.Uint32(uint32(os.O_RDONLY))
	openResponse := callNativeFDTestFrame(t, connection, descriptor, 2, fskitproto.OpOpen, openPayload.Data())
	if openResponse.Flags&fskitproto.FlagSharedWindow == 0 {
		t.Fatalf("virtual open flags = %#x, want shared window", openResponse.Flags)
	}
	decoder := fskitproto.NewDecoder(openResponse.Payload)
	handle, err := decoder.Uint64()
	if err != nil {
		t.Fatal(err)
	}
	windowBytes, err := decoder.Uint32()
	if err != nil || decoder.Done() != nil || int(windowBytes) != nativeFSKitSharedWindowBytes {
		t.Fatalf("shared window bytes=%d err=%v", windowBytes, err)
	}
	windowFD := receiveFDTestMarker(t, connection, fskitproto.SharedWindowFDMarker)
	defer unix.Close(windowFD)
	windowMapping, err := unix.Mmap(windowFD, 0, nativeSharedReadMappedLength(int(windowBytes)), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(windowMapping)

	assertWindowRead := func(requestID uint64, offset int64, length int, want []byte) {
		t.Helper()
		response := callNativeFDTestRead(t, connection, descriptor, requestID, handle, offset, length)
		if response.Flags&fskitproto.FlagSharedWindow == 0 || response.Flags&fskitproto.FlagSharedReadFD != 0 {
			t.Fatalf("shared window read flags = %#x", response.Flags)
		}
		count := decodeNativeFDTestReadCount(t, response)
		if count != len(want) || !bytes.Equal(windowMapping[:count], want) {
			t.Fatalf("shared window read offset=%d count=%d want=%d", offset, count, len(want))
		}
	}
	assertWindowRead(3, 0, 5<<20, source[:5<<20])
	assertWindowRead(4, 1<<20, 4<<20, source[1<<20:5<<20])
	assertWindowRead(5, int64(len(source)-73), 4<<20, source[len(source)-73:])
	callNativeFDTestHandle(t, connection, descriptor, 6, fskitproto.OpRelease, handle)
}

func TestNativeFSKitServerUsesSharedFileWindowForConcurrentPread(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	line := []byte("{\"shared_file_window\":true}\n")
	source := bytes.Repeat(line, (7<<20)/len(line)+1)
	virtualPath := "/archived_sessions/shared-file-window.jsonl"
	if err := filesystem.AddSessionAt("shared-file-window", virtualPath, mountSessionFixture(t, "shared-file-window", source)); err != nil {
		t.Fatal(err)
	}
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	_ = client.Close()
	defer stop()
	descriptor := readNativeFDTestDescriptor(t, root)
	connection := dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()
	callNativeFDTestHelloCapabilities(t, connection, descriptor, fskitproto.CapabilitySharedFileWindow)

	openPayload := fskitproto.NewEncoder(128)
	openPayload.String(virtualPath)
	openPayload.Uint32(uint32(os.O_RDONLY))
	openResponse := callNativeFDTestFrame(t, connection, descriptor, 2, fskitproto.OpOpen, openPayload.Data())
	if openResponse.Flags != fskitproto.FlagSharedFileWindow {
		t.Fatalf("virtual open flags = %#x, want shared-file window", openResponse.Flags)
	}
	decoder := fskitproto.NewDecoder(openResponse.Payload)
	handle, err := decoder.Uint64()
	if err != nil {
		t.Fatal(err)
	}
	windowBytes, err := decoder.Uint32()
	if err != nil || decoder.Done() != nil || int(windowBytes) != nativeFSKitSharedWindowBytes {
		t.Fatalf("shared-file window bytes=%d err=%v", windowBytes, err)
	}
	windowFD := receiveFDTestMarker(t, connection, fskitproto.SharedFileWindowFDMarker)
	defer unix.Close(windowFD)

	assertWindowRead := func(requestID uint64, offset int64, length int, want []byte) {
		t.Helper()
		response := callNativeFDTestRead(t, connection, descriptor, requestID, handle, offset, length)
		if response.Flags != fskitproto.FlagSharedFileWindow {
			t.Fatalf("shared-file window read flags = %#x", response.Flags)
		}
		count := decodeNativeFDTestReadCount(t, response)
		got := make([]byte, count)
		if n, err := unix.Pread(windowFD, got, 0); err != nil || n != count {
			t.Fatalf("pread shared-file window bytes=%d err=%v want=%d", n, err, count)
		}
		if count != len(want) || !bytes.Equal(got, want) {
			t.Fatalf("shared-file read offset=%d count=%d want=%d", offset, count, len(want))
		}
	}
	assertWindowRead(3, 0, 5<<20, source[:5<<20])
	assertWindowRead(4, 1<<20, 4<<20, source[1<<20:5<<20])
	assertWindowRead(5, int64(len(source)-79), 4<<20, source[len(source)-79:])
	callNativeFDTestHandle(t, connection, descriptor, 6, fskitproto.OpRelease, handle)
}

func TestNativeFSKitServerFallsBackFromPOSIXToSharedFileWindow(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	source := bytes.Repeat([]byte("{\"window_fallback\":true}\n"), (5<<20)/27+1)
	virtualPath := "/archived_sessions/file-window-fallback.jsonl"
	if err := filesystem.AddSessionAt("file-window-fallback", virtualPath, mountSessionFixture(t, "file-window-fallback", source)); err != nil {
		t.Fatal(err)
	}
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	_ = client.Close()
	defer stop()
	descriptor := readNativeFDTestDescriptor(t, root)
	connection := dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()
	callNativeFDTestHelloCapabilities(t, connection, descriptor, fskitproto.CapabilitySharedFileWindow|fskitproto.CapabilitySharedWindow)

	originalCreate := createNativeSharedReadObject
	createNativeSharedReadObject = func() (nativeSharedReadObject, error) {
		return nativeSharedReadObject{}, errors.New("injected POSIX window failure")
	}
	defer func() { createNativeSharedReadObject = originalCreate }()
	openPayload := fskitproto.NewEncoder(128)
	openPayload.String(virtualPath)
	openPayload.Uint32(uint32(os.O_RDONLY))
	openResponse := callNativeFDTestFrame(t, connection, descriptor, 2, fskitproto.OpOpen, openPayload.Data())
	if openResponse.Flags != fskitproto.FlagSharedFileWindow {
		t.Fatalf("POSIX window fallback flags = %#x, want shared-file window", openResponse.Flags)
	}
	decoder := fskitproto.NewDecoder(openResponse.Payload)
	handle, err := decoder.Uint64()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Uint32(); err != nil || decoder.Done() != nil {
		t.Fatalf("decode fallback window: %v", err)
	}
	windowFD := receiveFDTestMarker(t, connection, fskitproto.SharedFileWindowFDMarker)
	defer unix.Close(windowFD)
	callNativeFDTestHandle(t, connection, descriptor, 3, fskitproto.OpRelease, handle)
}

func TestNativeFSKitServerRecyclesPrewarmedPOSIXWindowsWithinBound(t *testing.T) {
	server := &nativeFSKitServer{}
	const windowBytes = 2 << 20
	if err := server.prewarmSharedMemoryWindows(2, windowBytes); err != nil {
		t.Fatal(err)
	}

	first, firstPrewarmed, err := server.acquireSharedMemoryWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	second, secondPrewarmed, err := server.acquireSharedMemoryWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !firstPrewarmed || !secondPrewarmed {
		t.Fatal("prewarmed POSIX windows were not acquired before recycle")
	}
	if !server.recycleSharedMemoryWindow(first) || !server.recycleSharedMemoryWindow(second) {
		t.Fatal("released POSIX windows were not recycled")
	}

	reused, reusedPrewarmed, err := server.acquireSharedMemoryWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reusedPrewarmed {
		t.Fatal("recycled POSIX window was not reused")
	}
	if !server.recycleSharedMemoryWindow(reused) {
		t.Fatal("reused POSIX window was not returned to the pool")
	}
	if err := server.closePrewarmedSharedMemoryWindows(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSharedFileWindowIsMode0600AndUnlinked(t *testing.T) {
	window, err := newNativeSharedFileWindow(2 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close()
	path := window.file.Name()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared-file window remained linked at %q: %v", path, err)
	}
	info, err := window.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("shared-file window mode = %v, want regular 0600", info.Mode())
	}
	want := bytes.Repeat([]byte("regular-window"), 4096)
	copy(window.mapping, want)
	got := make([]byte, len(want))
	if n, err := unix.Pread(int(window.file.Fd()), got, 0); err != nil || n != len(got) {
		t.Fatalf("pread anonymous shared-file window bytes=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("anonymous shared-file window bytes changed")
	}
}

func TestNativeFSKitServerPrewarmedWindowsHaveBoundedOwnership(t *testing.T) {
	server := &nativeFSKitServer{}
	const windowBytes = 2 << 20
	if err := server.prewarmSharedFileWindows(2, windowBytes); err != nil {
		t.Fatal(err)
	}
	server.sharedFileWindows.mu.Lock()
	if len(server.sharedFileWindows.windows) != 2 {
		server.sharedFileWindows.mu.Unlock()
		t.Fatalf("prewarmed windows=%d, want=2", len(server.sharedFileWindows.windows))
	}
	unusedFD := int(server.sharedFileWindows.windows[0].file.Fd())
	server.sharedFileWindows.mu.Unlock()

	acquired, prewarmed, err := server.acquireSharedFileWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !prewarmed {
		t.Fatal("prewarmed window was not consumed")
	}
	acquiredFD := int(acquired.file.Fd())
	if err := server.closePrewarmedSharedFileWindows(); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(unusedFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("unused prewarmed descriptor remained open: %v", err)
	}
	if _, err := unix.FcntlInt(uintptr(acquiredFD), unix.F_GETFD, 0); err != nil {
		t.Fatalf("pool close invalidated an acquired descriptor: %v", err)
	}
	if err := acquired.Close(); err != nil {
		t.Fatal(err)
	}

	fallback, prewarmed, err := server.acquireSharedFileWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if prewarmed {
		t.Fatal("empty pool reported a prewarmed window")
	}
	if err := fallback.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFSKitServerRecyclesSharedFileWindowsWithinBound(t *testing.T) {
	server := &nativeFSKitServer{}
	const windowBytes = 2 << 20
	if err := server.prewarmSharedFileWindows(2, windowBytes); err != nil {
		t.Fatal(err)
	}

	first, firstPrewarmed, err := server.acquireSharedFileWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	second, secondPrewarmed, err := server.acquireSharedFileWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !firstPrewarmed || !secondPrewarmed {
		t.Fatal("prewarmed windows were not acquired before recycle")
	}
	if !server.recycleSharedFileWindow(first) || !server.recycleSharedFileWindow(second) {
		t.Fatal("released shared-file windows were not recycled")
	}

	reused, reusedPrewarmed, err := server.acquireSharedFileWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reusedPrewarmed {
		t.Fatal("recycled shared-file window was not reused")
	}
	if !server.recycleSharedFileWindow(reused) {
		t.Fatal("reused shared-file window was not returned to the pool")
	}

	overflow, err := newNativeSharedFileWindow(windowBytes)
	if err != nil {
		t.Fatal(err)
	}
	overflowFD := int(overflow.file.Fd())
	if server.recycleSharedFileWindow(overflow) {
		t.Fatal("full pool accepted an overflow shared-file window")
	}
	if _, err := unix.FcntlInt(uintptr(overflowFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("overflow shared-file window remained open: %v", err)
	}
	if err := server.closePrewarmedSharedFileWindows(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFSKitServerFallsBackWhenSharedWindowPreparationFails(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	source := bytes.Repeat([]byte("{\"window_fallback\":true}\n"), (5<<20)/27+1)
	virtualPath := "/archived_sessions/window-fallback.jsonl"
	if err := filesystem.AddSessionAt("window-fallback", virtualPath, mountSessionFixture(t, "window-fallback", source)); err != nil {
		t.Fatal(err)
	}
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	_ = client.Close()
	defer stop()
	descriptor := readNativeFDTestDescriptor(t, root)
	connection := dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()
	callNativeFDTestHelloCapabilities(t, connection, descriptor, fskitproto.CapabilitySharedWindow)

	originalCreate := createNativeSharedReadObject
	createNativeSharedReadObject = func() (nativeSharedReadObject, error) {
		return nativeSharedReadObject{}, errors.New("injected shared window failure")
	}
	defer func() { createNativeSharedReadObject = originalCreate }()
	handle := openNativeFDTestPath(t, connection, descriptor, 2, virtualPath)
	response := callNativeFDTestRead(t, connection, descriptor, 3, handle, 0, nativeFSKitSharedReadMinimumBytes)
	if response.Flags != 0 {
		t.Fatalf("shared window fallback flags = %#x, want byte stream", response.Flags)
	}
	decoder := fskitproto.NewDecoder(response.Payload)
	got, err := decoder.Bytes(nativeFSKitSharedReadMinimumBytes)
	if err != nil || decoder.Done() != nil || !bytes.Equal(got, source[:nativeFSKitSharedReadMinimumBytes]) {
		t.Fatalf("shared window fallback bytes=%d err=%v", len(got), err)
	}
	callNativeFDTestHandle(t, connection, descriptor, 4, fskitproto.OpRelease, handle)
}

func TestNativeFSKitServerKeepsByteStreamWithoutSharedReadCapability(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	source := bytes.Repeat([]byte("{\"legacy_stream\":true}\n"), (5<<20)/24+1)
	virtualPath := "/archived_sessions/legacy-stream.jsonl"
	if err := filesystem.AddSessionAt("legacy-stream", virtualPath, mountSessionFixture(t, "legacy-stream", source)); err != nil {
		t.Fatal(err)
	}
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	_ = client.Close()
	defer stop()
	descriptor := readNativeFDTestDescriptor(t, root)
	connection := dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()
	callNativeFDTestHelloCapabilities(t, connection, descriptor, 0)

	handle := openNativeFDTestPath(t, connection, descriptor, 2, virtualPath)
	response := callNativeFDTestRead(t, connection, descriptor, 3, handle, 0, nativeFSKitSharedReadMinimumBytes)
	if response.Flags&fskitproto.FlagSharedReadFD != 0 {
		t.Fatalf("legacy read unexpectedly transferred shared FD: flags=%#x", response.Flags)
	}
	decoder := fskitproto.NewDecoder(response.Payload)
	got, err := decoder.Bytes(nativeFSKitSharedReadMinimumBytes)
	if err != nil || decoder.Done() != nil || !bytes.Equal(got, source[:nativeFSKitSharedReadMinimumBytes]) {
		t.Fatalf("legacy byte stream bytes=%d err=%v", len(got), err)
	}
	callNativeFDTestHandle(t, connection, descriptor, 4, fskitproto.OpRelease, handle)
}

func TestNativeFSKitServerFallsBackWhenSharedReadFDPreparationFails(t *testing.T) {
	root := t.TempDir()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(filepath.Join(root, "native"))
	source := bytes.Repeat([]byte("{\"shared_fallback\":true}\n"), (5<<20)/27+1)
	virtualPath := "/archived_sessions/shared-fallback.jsonl"
	if err := filesystem.AddSessionAt("shared-fallback", virtualPath, mountSessionFixture(t, "shared-fallback", source)); err != nil {
		t.Fatal(err)
	}
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	_ = client.Close()
	defer stop()
	descriptor := readNativeFDTestDescriptor(t, root)
	connection := dialNativeFDTestSocket(t, descriptor)
	defer connection.Close()
	callNativeFDTestHelloCapabilities(t, connection, descriptor, fskitproto.CapabilitySharedReadFD)
	handle := openNativeFDTestPath(t, connection, descriptor, 2, virtualPath)

	originalCreate := createNativeSharedReadObject
	createNativeSharedReadObject = func() (nativeSharedReadObject, error) {
		return nativeSharedReadObject{}, errors.New("injected shared read object failure")
	}
	defer func() { createNativeSharedReadObject = originalCreate }()
	response := callNativeFDTestRead(t, connection, descriptor, 3, handle, 0, nativeFSKitSharedReadMinimumBytes)
	if response.Flags&fskitproto.FlagSharedReadFD != 0 {
		t.Fatalf("failed shared read preparation still set FD flag: %#x", response.Flags)
	}
	decoder := fskitproto.NewDecoder(response.Payload)
	got, err := decoder.Bytes(nativeFSKitSharedReadMinimumBytes)
	if err != nil || decoder.Done() != nil || !bytes.Equal(got, source[:nativeFSKitSharedReadMinimumBytes]) {
		t.Fatalf("shared read fallback bytes=%d err=%v", len(got), err)
	}
	callNativeFDTestHandle(t, connection, descriptor, 4, fskitproto.OpRelease, handle)
}

func TestPrepareNativeSharedReadFDUnlinksBeforePopulation(t *testing.T) {
	directory := t.TempDir()
	var temporaryPath string
	originalCreate := createNativeSharedReadObject
	createNativeSharedReadObject = func() (nativeSharedReadObject, error) {
		file, err := os.CreateTemp(directory, ".codexfold-shared-read-*")
		if err == nil {
			temporaryPath = file.Name()
		}
		return nativeSharedReadObject{file: file, unlink: func() error { return os.Remove(temporaryPath) }}, err
	}
	defer func() { createNativeSharedReadObject = originalCreate }()
	want := bytes.Repeat([]byte("mapped-shared-read"), 1024)
	file, count, err := prepareNativeSharedReadFD(len(want), func(mapping []byte) (int, error) {
		if _, statErr := os.Stat(temporaryPath); !errors.Is(statErr, os.ErrNotExist) {
			return 0, errors.New("shared read file remained linked during population")
		}
		return copy(mapping, want), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != len(want) {
		t.Fatalf("populated bytes = %d, want %d", count, len(want))
	}
	assertMappedNativeFDTestBytes(t, int(file.Fd()), want)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("shared read left %d temporary directory entries", len(entries))
	}
}

func TestNativePOSIXSharedMemoryCanBeUnlinkedMappedAndTransferred(t *testing.T) {
	object, err := newNativePOSIXSharedMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer object.file.Close()
	if err := object.unlink(); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("posix-shared-memory"), 1024)
	mappedLength := nativeSharedReadMappedLength(len(want))
	if err := object.file.Truncate(int64(mappedLength)); err != nil {
		t.Fatal(err)
	}
	mapping, err := unix.Mmap(int(object.file.Fd()), 0, mappedLength, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	copy(mapping, want)
	if err := unix.Munmap(mapping); err != nil {
		t.Fatal(err)
	}
	assertMappedNativeFDTestBytes(t, int(object.file.Fd()), want)
}

func TestPrepareNativeSharedReadFDUsesPOSIXSharedMemory(t *testing.T) {
	want := bytes.Repeat([]byte("prepared-posix-shared-memory"), 1<<18)
	file, count, err := prepareNativeSharedReadFD(len(want), func(mapping []byte) (int, error) {
		return copy(mapping, want), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if count != len(want) {
		t.Fatalf("prepared bytes = %d, want %d", count, len(want))
	}
	assertMappedNativeFDTestBytes(t, int(file.Fd()), want)
}

func unixSocketOption(t *testing.T, connection *net.UnixConn, option int) int {
	t.Helper()
	raw, err := connection.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var value int
	var socketErr error
	if err := raw.Control(func(descriptor uintptr) {
		value, socketErr = syscall.GetsockoptInt(int(descriptor), syscall.SOL_SOCKET, option)
	}); err != nil {
		t.Fatal(err)
	}
	if socketErr != nil {
		t.Fatal(socketErr)
	}
	return value
}

func TestNativeFSKitServerFallsBackWhenReadFDPreparationFailsBeforeResponse(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	wirePath := "/sessions/2026/07/17/fallback.jsonl"
	nativePath := nativePathFromRoot(nativeRoot, wirePath)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativePath, []byte("{\"record\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	coreHandle, errno := filesystem.Open(wirePath, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open native file errno=%v", errno)
	}
	defer filesystem.Release(coreHandle)
	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}

	serverConnection, peerConnection := net.Pipe()
	_ = peerConnection.Close()
	defer serverConnection.Close()
	connection := &nativeFSKitConnection{
		server: &nativeFSKitServer{
			filesystem: filesystem,
			generation: 1,
			maxPayload: fskitproto.DefaultMaxPayload,
		},
		conn: serverConnection,
		handles: map[uint64]*nativeFSKitHandle{
			7: {coreHandle: coreHandle, path: wirePath, flags: os.O_RDONLY, snapshotFloor: -1},
		},
		capabilities: fskitproto.CapabilityNativeReadFD,
	}
	payload := fskitproto.NewEncoder(8)
	payload.Uint64(7)
	handled, responseWritten, err := connection.writeOpenResponseWithNativeFD(fskitproto.Frame{
		Kind: fskitproto.KindResponse, Op: fskitproto.OpOpen, RequestID: 2, Generation: 1, Payload: payload.Data(),
	})
	if err != nil || handled || responseWritten {
		t.Fatalf("native FD pre-send failure handled=%t response_written=%t err=%v, want byte-stream fallback", handled, responseWritten, err)
	}
}

func dialNativeFDTestSocket(t *testing.T, descriptor fskitproto.Descriptor) *net.UnixConn {
	t.Helper()
	address := &net.UnixAddr{Name: descriptor.SocketPath, Net: "unix"}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialUnix("unix", nil, address)
		if err == nil {
			return connection
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dial native FD test socket: %s", descriptor.SocketPath)
	return nil
}

func callNativeFDTestHello(t *testing.T, connection *net.UnixConn, descriptor fskitproto.Descriptor, capability bool) {
	t.Helper()
	var capabilities uint32
	if capability {
		capabilities = fskitproto.CapabilityNativeReadFD
	}
	callNativeFDTestHelloCapabilities(t, connection, descriptor, capabilities)
}

func callNativeFDTestHelloCapabilities(t *testing.T, connection *net.UnixConn, descriptor fskitproto.Descriptor, capabilities uint32) {
	t.Helper()
	encoder := fskitproto.NewEncoder(64)
	encoder.Bytes(descriptor.Token)
	if capabilities != 0 {
		encoder.Uint32(capabilities)
	}
	response := callNativeFDTestFrame(t, connection, descriptor, 1, fskitproto.OpHello, encoder.Data())
	decoder := fskitproto.NewDecoder(response.Payload)
	if maxPayload, err := decoder.Uint32(); err != nil || maxPayload < 4096 {
		t.Fatalf("hello max payload=%d err=%v", maxPayload, err)
	}
	if _, err := decoder.Uint64(); err != nil || decoder.Done() != nil {
		t.Fatalf("hello response decode: %v", err)
	}
}

func readNativeFDTestDescriptor(t *testing.T, root string) fskitproto.Descriptor {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "resource.bin"))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := fskitproto.DecodeDescriptor(data)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func openNativeFDTestPath(t *testing.T, connection *net.UnixConn, descriptor fskitproto.Descriptor, requestID uint64, path string) uint64 {
	t.Helper()
	payload := fskitproto.NewEncoder(128)
	payload.String(path)
	payload.Uint32(uint32(os.O_RDONLY))
	response := callNativeFDTestFrame(t, connection, descriptor, requestID, fskitproto.OpOpen, payload.Data())
	decoder := fskitproto.NewDecoder(response.Payload)
	handle, err := decoder.Uint64()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("decode open handle=%d err=%v", handle, err)
	}
	return handle
}

func callNativeFDTestRead(t *testing.T, connection *net.UnixConn, descriptor fskitproto.Descriptor, requestID, handle uint64, offset int64, length int) fskitproto.Frame {
	t.Helper()
	payload := fskitproto.NewEncoder(24)
	payload.Uint64(handle)
	payload.Int64(offset)
	payload.Uint32(uint32(length))
	return callNativeFDTestFrame(t, connection, descriptor, requestID, fskitproto.OpRead, payload.Data())
}

func decodeNativeFDTestReadCount(t *testing.T, response fskitproto.Frame) int {
	t.Helper()
	decoder := fskitproto.NewDecoder(response.Payload)
	count, err := decoder.Uint32()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("decode shared read count=%d err=%v", count, err)
	}
	return int(count)
}

func callNativeFDTestFrame(t *testing.T, connection *net.UnixConn, descriptor fskitproto.Descriptor, requestID uint64, operation fskitproto.Op, payload []byte) fskitproto.Frame {
	t.Helper()
	if err := fskitproto.WriteFrame(connection, fskitproto.Frame{
		Kind: fskitproto.KindRequest, Op: operation, RequestID: requestID,
		Generation: func() uint64 {
			if operation == fskitproto.OpHello {
				return 0
			}
			return descriptor.Generation
		}(), Payload: payload,
	}, fskitproto.DefaultMaxPayload); err != nil {
		t.Fatal(err)
	}
	response, err := fskitproto.ReadFrame(connection, fskitproto.DefaultMaxPayload)
	if err != nil {
		t.Fatalf("read %v response: %v", operation, err)
	}
	if response.Kind != fskitproto.KindResponse || response.Op != operation || response.RequestID != requestID {
		t.Fatalf("unexpected %v response: %#v", operation, response)
	}
	if response.Status != 0 {
		t.Fatalf("%v response status=%d", operation, response.Status)
	}
	return response
}

func callNativeFDTestHandle(t *testing.T, connection *net.UnixConn, descriptor fskitproto.Descriptor, requestID uint64, operation fskitproto.Op, handle uint64) {
	t.Helper()
	payload := fskitproto.NewEncoder(8)
	payload.Uint64(handle)
	callNativeFDTestFrame(t, connection, descriptor, requestID, operation, payload.Data())
}

func receiveNativeFDTestMarker(t *testing.T, connection *net.UnixConn) int {
	t.Helper()
	return receiveFDTestMarker(t, connection, fskitproto.NativeReadFDMarker)
}

func receiveFDTestMarker(t *testing.T, connection *net.UnixConn, expectedMarker byte) int {
	t.Helper()
	marker := make([]byte, 1)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, flags, _, err := connection.ReadMsgUnix(marker, oob)
	if err != nil {
		t.Fatalf("receive native FD marker: %v", err)
	}
	if n != 1 || marker[0] != expectedMarker || flags&(syscall.MSG_CTRUNC|syscall.MSG_TRUNC) != 0 {
		t.Fatalf("native FD marker n=%d marker=%#x flags=%#x", n, marker, flags)
	}
	messages, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(messages) != 1 {
		t.Fatalf("parse native FD control messages: count=%d err=%v", len(messages), err)
	}
	fds, err := syscall.ParseUnixRights(&messages[0])
	if err != nil || len(fds) != 1 || fds[0] < 0 {
		t.Fatalf("parse native FD rights: fds=%v err=%v", fds, err)
	}
	return fds[0]
}

func assertMappedNativeFDTestBytes(t *testing.T, fd int, want []byte, maximumBytes ...int) {
	t.Helper()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		t.Fatal(err)
	}
	mappedLength := nativeSharedReadMappedLength(len(want))
	maximumLength := mappedLength
	if len(maximumBytes) != 0 {
		maximumLength = nativeSharedReadMappedLength(maximumBytes[0])
	}
	if info.Size < int64(len(want)) || info.Size > int64(maximumLength) {
		t.Fatalf("shared FD size = %d, want between %d and %d", info.Size, len(want), maximumLength)
	}
	mapping, err := unix.Mmap(fd, 0, mappedLength, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mapping[:len(want)], want) {
		_ = unix.Munmap(mapping)
		t.Fatalf("mapped shared FD bytes differ: got=%d want=%d", len(mapping), len(want))
	}
	if err := unix.Munmap(mapping); err != nil {
		t.Fatal(err)
	}
}
