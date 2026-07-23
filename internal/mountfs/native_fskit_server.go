package mountfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jstar0/codexfold/internal/buildid"
	"github.com/jstar0/codexfold/internal/fskitproto"
	"github.com/jstar0/codexfold/internal/mountid"
)

type NativeFSKitServerOptions struct {
	SocketPath                 string
	ResourcePath               string
	Token                      []byte
	Generation                 uint64
	MaxPayload                 uint32
	BuildSHA256                string
	Recorder                   func(string)
	PrewarmSharedMemoryWindows int
	PrewarmSharedFileWindows   int
}

const (
	nativeFSKitBufferedReadChunkBytes = 4 << 20
	nativeFSKitSocketBufferBytes      = 4 << 20
	nativeFSKitSharedReadMinimumBytes = 4 << 20
	nativeFSKitSharedWindowBytes      = 16 << 20
	nativeFSKitMaximumPrewarmWindows  = 4
)

type nativeFSKitServer struct {
	filesystem          *Filesystem
	token               []byte
	generation          uint64
	maxPayload          uint32
	recorder            func(string)
	health              []byte
	startedAt           time.Time
	nodes               nativeFSKitNodes
	sharedMemoryWindows nativeSharedFileWindowPool
	sharedFileWindows   nativeSharedFileWindowPool
}

type nativeSharedFileWindowPool struct {
	mu          sync.Mutex
	windows     []*nativeSharedReadWindow
	limit       int
	windowBytes int
}

type nativeFSKitNodes struct {
	mu      sync.Mutex
	version uint64
	next    uint64
	byPath  map[string]nativeFSKitNode
}

type nativeFSKitNode struct {
	id       uint64
	objectID string
}

type nativeFSKitConnection struct {
	server       *nativeFSKitServer
	conn         net.Conn
	handles      map[uint64]*nativeFSKitHandle
	nextHandle   uint64
	capabilities uint32
}

type nativeFSKitHandle struct {
	coreHandle       uint64
	path             string
	flags            int
	snapshotWrite    bool
	snapshotFloor    int64
	health           bool
	sharedWindow     *nativeSharedReadWindow
	sharedWindowFlag uint32
}

func ServeNativeFSKit(ctx context.Context, filesystem *Filesystem, options NativeFSKitServerOptions) error {
	if filesystem == nil {
		return errors.New("FSKit server filesystem is required")
	}
	if options.SocketPath == "" || !filepath.IsAbs(options.SocketPath) {
		return errors.New("FSKit server socket path must be absolute")
	}
	if len(options.SocketPath) >= 104 {
		return errors.New("FSKit server socket path exceeds the macOS Unix socket limit")
	}
	if options.ResourcePath == "" || !filepath.IsAbs(options.ResourcePath) {
		return errors.New("FSKit resource path must be absolute")
	}
	if fskitproto.UsesDirectoryResource(options.ResourcePath) {
		relativeSocket, err := filepath.Rel(filepath.Clean(options.ResourcePath), filepath.Clean(options.SocketPath))
		if err != nil || relativeSocket == "." || relativeSocket == ".." || strings.HasPrefix(relativeSocket, ".."+string(filepath.Separator)) {
			return errors.New("directory FSKit resource requires its Unix socket inside the resource directory")
		}
	}
	if options.MaxPayload == 0 {
		options.MaxPayload = fskitproto.DefaultMaxPayload
	}
	if options.PrewarmSharedMemoryWindows < 0 || options.PrewarmSharedMemoryWindows > nativeFSKitMaximumPrewarmWindows {
		return fmt.Errorf("FSKit shared-memory window prewarm count must be between 0 and %d", nativeFSKitMaximumPrewarmWindows)
	}
	if options.PrewarmSharedFileWindows < 0 || options.PrewarmSharedFileWindows > nativeFSKitMaximumPrewarmWindows {
		return fmt.Errorf("FSKit shared-file window prewarm count must be between 0 and %d", nativeFSKitMaximumPrewarmWindows)
	}
	if len(options.Token) == 0 {
		options.Token = make([]byte, 32)
		if _, err := rand.Read(options.Token); err != nil {
			return fmt.Errorf("generate FSKit authentication token: %w", err)
		}
	}
	if len(options.Token) < 16 || len(options.Token) > 256 {
		return errors.New("FSKit authentication token must contain 16 to 256 bytes")
	}
	if options.Generation == 0 {
		var generation [8]byte
		if _, err := rand.Read(generation[:]); err != nil {
			return fmt.Errorf("generate FSKit mount generation: %w", err)
		}
		for _, value := range generation {
			options.Generation = options.Generation<<8 | uint64(value)
		}
		if options.Generation == 0 {
			options.Generation = 1
		}
	}
	if options.BuildSHA256 == "" {
		var err error
		options.BuildSHA256, err = buildid.CurrentSHA256()
		if err != nil {
			return fmt.Errorf("hash native FSKit daemon executable: %w", err)
		}
	}
	health, err := mountid.New(options.BuildSHA256)
	if err != nil {
		return fmt.Errorf("generate native FSKit mount identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(options.SocketPath), 0o700); err != nil {
		return fmt.Errorf("create FSKit socket directory: %w", err)
	}
	if err := removeStaleUnixSocket(options.SocketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", options.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on FSKit socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(options.SocketPath)
	if err := os.Chmod(options.SocketPath, 0o600); err != nil {
		return fmt.Errorf("restrict FSKit socket: %w", err)
	}
	server := &nativeFSKitServer{
		filesystem: filesystem,
		token:      append([]byte(nil), options.Token...),
		generation: options.Generation,
		maxPayload: options.MaxPayload,
		recorder:   options.Recorder,
		health:     []byte(health),
		startedAt:  time.Now(),
		nodes: nativeFSKitNodes{
			version: filesystem.NamespaceVersion(),
			next:    4,
			byPath:  map[string]nativeFSKitNode{"/": {id: 2}},
		},
	}
	windowBytes := min(nativeFSKitSharedWindowBytes, int(options.MaxPayload)-4)
	if err := server.prewarmSharedMemoryWindows(options.PrewarmSharedMemoryWindows, windowBytes); err != nil {
		server.record(fmt.Sprintf("io=shared_window_prewarm_error error=%q", err.Error()))
	}
	if err := server.prewarmSharedFileWindows(options.PrewarmSharedFileWindows, windowBytes); err != nil {
		server.record(fmt.Sprintf("io=shared_file_window_prewarm_error error=%q", err.Error()))
	}
	defer func() {
		if err := errors.Join(server.closePrewarmedSharedMemoryWindows(), server.closePrewarmedSharedFileWindows()); err != nil {
			server.record(fmt.Sprintf("io=shared_file_window_pool_close_error error=%q", err.Error()))
		}
	}()
	descriptor, err := fskitproto.EncodeDescriptor(fskitproto.Descriptor{
		Generation: options.Generation,
		SocketPath: options.SocketPath,
		Token:      options.Token,
	})
	if err != nil {
		return err
	}
	if err := writeNativeFSKitResource(options.ResourcePath, descriptor); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			return fmt.Errorf("accept FSKit connection: %w", err)
		}
		if err := configureNativeFSKitSocket(connection); err != nil {
			server.record(fmt.Sprintf("connection_buffer_error error=%q", err.Error()))
		}
		go (&nativeFSKitConnection{server: server, conn: connection, handles: make(map[uint64]*nativeFSKitHandle), nextHandle: 1}).serve()
	}
}

func configureNativeFSKitSocket(connection net.Conn) error {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return nil
	}
	return errors.Join(
		unixConnection.SetReadBuffer(nativeFSKitSocketBufferBytes),
		unixConnection.SetWriteBuffer(nativeFSKitSocketBufferBytes),
	)
}

func removeStaleUnixSocket(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect FSKit socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("FSKit socket path exists and is not a Unix socket")
	}
	connection, dialErr := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return errors.New("FSKit socket is already active")
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("remove stale FSKit socket: %w", err)
	}
	return nil
}

func writeNativeFSKitResource(resourcePath string, data []byte) error {
	descriptorPath, err := fskitproto.ResourceDescriptorPath(resourcePath)
	if err != nil {
		return err
	}
	if fskitproto.UsesDirectoryResource(resourcePath) {
		if err := os.MkdirAll(resourcePath, 0o700); err != nil {
			return fmt.Errorf("create FSKit resource directory: %w", err)
		}
		if err := os.Chmod(resourcePath, 0o700); err != nil {
			return fmt.Errorf("restrict FSKit resource directory: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(descriptorPath), 0o700); err != nil {
		return fmt.Errorf("create FSKit resource directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(descriptorPath), ".codexfold-fskit-resource-*")
	if err != nil {
		return fmt.Errorf("create FSKit resource: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict FSKit resource: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write FSKit resource: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync FSKit resource: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close FSKit resource: %w", err)
	}
	if err := os.Rename(temporaryPath, descriptorPath); err != nil {
		return fmt.Errorf("publish FSKit resource: %w", err)
	}
	committed = true
	return nil
}

func (c *nativeFSKitConnection) serve() {
	defer c.conn.Close()
	defer c.releaseHandles()
	authenticated := false
	for {
		request, err := fskitproto.ReadFrame(c.conn, c.server.maxPayload)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				c.server.record(fmt.Sprintf("connection_error error=%q", err.Error()))
			}
			return
		}
		response := fskitproto.Frame{
			Kind:       fskitproto.KindResponse,
			Op:         request.Op,
			RequestID:  request.RequestID,
			Generation: c.server.generation,
		}
		if request.Kind != fskitproto.KindRequest {
			response.Status = int32(syscall.EPROTO)
		} else if !authenticated {
			if request.Op != fskitproto.OpHello {
				response.Status = int32(syscall.EACCES)
			} else {
				response.Payload, response.Status = c.hello(request.Payload)
				authenticated = response.Status == 0
			}
		} else if request.Generation != c.server.generation {
			response.Status = int32(syscall.ESTALE)
		} else {
			c.recordReadRequest(request.Payload, request.Op)
			streamed, responseWritten, streamStatus, streamErr := c.streamRead(request)
			if streamErr != nil {
				if responseWritten {
					return
				}
				response.Status = int32(errnoFor(streamErr))
			} else if streamed && responseWritten {
				c.server.record(fmt.Sprintf("operation=%s request=%d status=%d payload=%d", nativeFSKitOperationName(request.Op), request.RequestID, 0, len(request.Payload)))
				continue
			} else if streamed {
				response.Status = streamStatus
			} else {
				response.Payload, response.Status = c.dispatch(request.Op, request.Payload)
			}
		}
		if response.Status == 0 && request.Op == fskitproto.OpOpen && authenticated {
			handled, responseWritten, transferErr := c.writeOpenResponseWithOptimizedFD(response)
			if transferErr != nil {
				if responseWritten {
					return
				}
				response.Status = int32(errnoFor(transferErr))
				response.Payload = nil
			} else if handled && responseWritten {
				c.server.record(fmt.Sprintf("operation=%s request=%d status=%d payload=%d", nativeFSKitOperationName(request.Op), request.RequestID, 0, len(request.Payload)))
				continue
			}
		}
		c.server.record(fmt.Sprintf("operation=%s request=%d status=%d payload=%d", nativeFSKitOperationName(request.Op), request.RequestID, response.Status, len(request.Payload)))
		if err := fskitproto.WriteFrame(c.conn, response, c.server.maxPayload); err != nil {
			return
		}
		if !authenticated || response.Status == int32(syscall.ESTALE) {
			return
		}
	}
}

// streamRead writes successful read responses without copying the payload
// through the generic frame encoder. Stable native files use sendfile; virtual
// files and pending native appends use bounded chunks.
func (c *nativeFSKitConnection) streamRead(request fskitproto.Frame) (handled bool, responseWritten bool, status int32, err error) {
	if request.Op != fskitproto.OpRead {
		return false, false, 0, nil
	}
	decoder := fskitproto.NewDecoder(request.Payload)
	handle, offset, length, errno := decodeRead(decoder, c.server.maxPayload)
	if errno != 0 {
		return false, false, 0, nil
	}
	handleState, exists := c.handles[handle]
	if !exists || handleState.health {
		return false, false, 0, nil
	}
	response := fskitproto.Frame{
		Kind:       fskitproto.KindResponse,
		Op:         request.Op,
		RequestID:  request.RequestID,
		Generation: c.server.generation,
	}
	started := false
	startedAt := time.Now()
	handled = false
	n := 0
	var streamErr error
	if nativeFileStreamingAvailable() {
		handled, n, streamErr = c.server.filesystem.StreamNativeRead(handleState.coreHandle, offset, length, func(file *os.File, fileOffset int64, count int) (int, error) {
			started = true
			if err := fskitproto.WriteFrameHeader(c.conn, response, 4+count, c.server.maxPayload); err != nil {
				return 0, err
			}
			var lengthPrefix [4]byte
			binary.LittleEndian.PutUint32(lengthPrefix[:], uint32(count))
			if err := fskitproto.WriteFramePayload(c.conn, lengthPrefix[:]); err != nil {
				return 0, err
			}
			return sendNativeFile(c.conn, file, fileOffset, count)
		})
	}
	if !handled {
		if handleState.sharedWindow != nil && handleState.sharedWindowFlag != 0 && length <= handleState.sharedWindow.capacity {
			windowHandled, windowWritten, windowBytes, windowErr := c.streamSharedReadWindow(
				response,
				handleState,
				offset,
				length,
			)
			if windowHandled {
				transport := "shared_window"
				if handleState.sharedWindowFlag == fskitproto.FlagSharedFileWindow {
					transport = "shared_file_window"
				}
				c.server.record(fmt.Sprintf("io=read_result handle=%d offset=%d bytes=%d duration_ns=%d transport=%s", handle, offset, windowBytes, time.Since(startedAt).Nanoseconds(), transport))
				return true, windowWritten, 0, windowErr
			}
		}
		if c.capabilities&fskitproto.CapabilitySharedReadFD != 0 &&
			nativeSharedReadFDAvailable() && length >= nativeFSKitSharedReadMinimumBytes {
			sharedHandled, sharedWritten, sharedBytes, sharedErr := c.streamSharedReadFD(
				response,
				handleState.coreHandle,
				offset,
				length,
			)
			if sharedHandled {
				c.server.record(fmt.Sprintf("io=read_result handle=%d offset=%d bytes=%d duration_ns=%d transport=shared_fd", handle, offset, sharedBytes, time.Since(startedAt).Nanoseconds()))
				return true, sharedWritten, 0, sharedErr
			}
		}
		startedAt = time.Now()
		n, streamErr = c.server.filesystem.StreamBufferedRead(handleState.coreHandle, offset, length, nativeFSKitBufferedReadChunkBytes, func(total int, chunk []byte) error {
			if !started {
				if err := fskitproto.WriteFrameHeader(c.conn, response, 4+total, c.server.maxPayload); err != nil {
					return err
				}
				var lengthPrefix [4]byte
				binary.LittleEndian.PutUint32(lengthPrefix[:], uint32(total))
				if err := fskitproto.WriteFramePayload(c.conn, lengthPrefix[:]); err != nil {
					return err
				}
				started = true
			}
			return fskitproto.WriteFramePayload(c.conn, chunk)
		})
		c.server.record(fmt.Sprintf("io=read_result handle=%d offset=%d bytes=%d duration_ns=%d", handle, offset, n, time.Since(startedAt).Nanoseconds()))
		if streamErr != nil {
			return true, started, 0, streamErr
		}
		return true, started, 0, nil
	}
	c.server.record(fmt.Sprintf("io=read_result handle=%d offset=%d bytes=%d duration_ns=%d", handle, offset, n, time.Since(startedAt).Nanoseconds()))
	if streamErr != nil {
		return true, started, 0, streamErr
	}
	return true, started, 0, nil
}

func (c *nativeFSKitConnection) streamSharedReadWindow(response fskitproto.Frame, handle *nativeFSKitHandle, offset int64, length int) (handled bool, responseWritten bool, n int, err error) {
	if length < 0 || length > handle.sharedWindow.capacity {
		return true, false, 0, errors.New("shared window read exceeded negotiated capacity")
	}
	n, readErrno := c.server.filesystem.Read(handle.coreHandle, handle.sharedWindow.mapping[:length], offset)
	if readErrno != 0 {
		return true, false, n, readErrno
	}
	if n < 0 || n > length {
		return true, false, n, errors.New("shared window returned an invalid byte count")
	}
	encoder := fskitproto.NewEncoder(4)
	encoder.Uint32(uint32(n))
	if handle.sharedWindowFlag != fskitproto.FlagSharedWindow && handle.sharedWindowFlag != fskitproto.FlagSharedFileWindow {
		return true, false, n, errors.New("shared window has an invalid transport flag")
	}
	response.Flags |= handle.sharedWindowFlag
	response.Payload = encoder.Data()
	if err := fskitproto.WriteFrame(c.conn, response, c.server.maxPayload); err != nil {
		return true, true, n, err
	}
	return true, true, n, nil
}

func (c *nativeFSKitConnection) streamSharedReadFD(response fskitproto.Frame, coreHandle uint64, offset int64, length int) (handled bool, responseWritten bool, n int, err error) {
	file, n, prepareErr := prepareNativeSharedReadFD(length, func(mapping []byte) (int, error) {
		read, readErrno := c.server.filesystem.Read(coreHandle, mapping, offset)
		if readErrno != 0 {
			return read, readErrno
		}
		if read < 0 || read > len(mapping) {
			return read, errors.New("shared read returned an invalid byte count")
		}
		return read, nil
	})
	if prepareErr != nil {
		c.server.record(fmt.Sprintf("io=shared_read_fallback offset=%d bytes=%d error=%q", offset, length, prepareErr.Error()))
		return false, false, 0, nil
	}
	defer file.Close()
	encoder := fskitproto.NewEncoder(4)
	encoder.Uint32(uint32(n))
	response.Payload = encoder.Data()
	if n == 0 {
		if err := fskitproto.WriteFrame(c.conn, response, c.server.maxPayload); err != nil {
			return true, true, 0, err
		}
		return true, true, 0, nil
	}
	response.Flags |= fskitproto.FlagSharedReadFD
	if err := fskitproto.WriteFrame(c.conn, response, c.server.maxPayload); err != nil {
		return true, true, n, err
	}
	if err := sendSharedReadFD(c.conn, file); err != nil {
		return true, true, n, err
	}
	return true, true, n, nil
}

func (c *nativeFSKitConnection) recordReadRequest(payload []byte, operation fskitproto.Op) {
	if operation != fskitproto.OpRead {
		return
	}
	decoder := fskitproto.NewDecoder(payload)
	handle, offset, length, errno := decodeRead(decoder, c.server.maxPayload)
	if errno == 0 {
		c.server.record(fmt.Sprintf("io=read handle=%d offset=%d bytes=%d", handle, offset, length))
	}
}

func (c *nativeFSKitConnection) hello(payload []byte) ([]byte, int32) {
	decoder := fskitproto.NewDecoder(payload)
	token, err := decoder.Bytes(256)
	if err != nil || len(token) != len(c.server.token) || subtle.ConstantTimeCompare(token, c.server.token) != 1 {
		return nil, int32(syscall.EACCES)
	}
	var capabilities uint32
	if decoder.Remaining() != 0 {
		capabilities, err = decoder.Uint32()
		if err != nil {
			return nil, int32(syscall.EINVAL)
		}
	}
	if decoder.Done() != nil {
		return nil, int32(syscall.EINVAL)
	}
	c.capabilities = capabilities
	encoder := fskitproto.NewEncoder(16)
	encoder.Uint32(c.server.maxPayload)
	encoder.Uint64(c.server.filesystem.NamespaceVersion())
	if capabilities&fskitproto.CapabilityContentGeneration != 0 {
		encoder.Uint32(capabilities & fskitproto.CapabilityContentGeneration)
	}
	return encoder.Data(), 0
}

func (c *nativeFSKitConnection) writeOpenResponseWithNativeFD(response fskitproto.Frame) (handled bool, responseWritten bool, err error) {
	if c.capabilities&fskitproto.CapabilityNativeReadFD == 0 {
		return false, false, nil
	}
	decoder := fskitproto.NewDecoder(response.Payload)
	wireHandle, decodeErr := decoder.Uint64()
	if decodeErr != nil || decoder.Done() != nil {
		return false, false, nil
	}
	handleState, exists := c.handles[wireHandle]
	if !exists || handleState.health || handleState.flags&(os.O_WRONLY|os.O_RDWR) != 0 {
		return false, false, nil
	}
	started := false
	handled, transferErr := c.server.filesystem.WithNativeReadFD(handleState.coreHandle, func(file *os.File) error {
		started = true
		response.Flags |= fskitproto.FlagNativeReadFD
		if err := fskitproto.WriteFrame(c.conn, response, c.server.maxPayload); err != nil {
			return err
		}
		return sendNativeReadFD(c.conn, file)
	})
	if !handled {
		return false, false, nil
	}
	if transferErr != nil && !started {
		return false, false, nil
	}
	return true, started, transferErr
}

func (c *nativeFSKitConnection) writeOpenResponseWithOptimizedFD(response fskitproto.Frame) (handled bool, responseWritten bool, err error) {
	handled, responseWritten, err = c.writeOpenResponseWithNativeFD(response)
	if handled || err != nil {
		return handled, responseWritten, err
	}
	if c.capabilities&(fskitproto.CapabilitySharedWindow|fskitproto.CapabilitySharedFileWindow) == 0 || !nativeSharedReadFDAvailable() {
		return false, false, nil
	}
	decoder := fskitproto.NewDecoder(response.Payload)
	wireHandle, decodeErr := decoder.Uint64()
	if decodeErr != nil || decoder.Done() != nil {
		return false, false, nil
	}
	handleState, exists := c.handles[wireHandle]
	if !exists || handleState.health || handleState.flags&(os.O_WRONLY|os.O_RDWR) != 0 {
		return false, false, nil
	}
	windowBytes := min(nativeFSKitSharedWindowBytes, int(c.server.maxPayload)-4)
	var window *nativeSharedReadWindow
	var windowFlag uint32
	if c.capabilities&fskitproto.CapabilitySharedWindow != 0 {
		sharedWindow, prewarmed, createErr := c.server.acquireSharedMemoryWindow(windowBytes)
		if createErr != nil {
			c.server.record(fmt.Sprintf("io=shared_window_fallback error=%q", createErr.Error()))
		} else {
			window = sharedWindow
			windowFlag = fskitproto.FlagSharedWindow
			if prewarmed {
				c.server.record("io=shared_window_pool_hit")
			}
		}
	}
	if window == nil && c.capabilities&fskitproto.CapabilitySharedFileWindow != 0 && nativeSharedFileWindowAvailable() {
		fileWindow, prewarmed, createErr := c.server.acquireSharedFileWindow(windowBytes)
		if createErr != nil {
			c.server.record(fmt.Sprintf("io=shared_file_window_fallback error=%q", createErr.Error()))
		} else {
			window = fileWindow
			windowFlag = fskitproto.FlagSharedFileWindow
			if prewarmed {
				c.server.record("io=shared_file_window_pool_hit")
			}
		}
	}
	if window == nil {
		return false, false, nil
	}
	handleState.sharedWindow = window
	handleState.sharedWindowFlag = windowFlag
	encoder := fskitproto.NewEncoder(12)
	encoder.Uint64(wireHandle)
	encoder.Uint32(uint32(window.capacity))
	response.Payload = encoder.Data()
	response.Flags |= windowFlag
	if err := fskitproto.WriteFrame(c.conn, response, c.server.maxPayload); err != nil {
		return true, true, err
	}
	var transferErr error
	if windowFlag == fskitproto.FlagSharedFileWindow {
		transferErr = sendSharedFileWindowFD(c.conn, window.file)
	} else {
		transferErr = sendSharedWindowFD(c.conn, window.file)
	}
	if transferErr != nil {
		return true, true, transferErr
	}
	return true, true, nil
}

func (s *nativeFSKitServer) prewarmSharedFileWindows(count int, length int) error {
	if count == 0 || !nativeSharedFileWindowAvailable() {
		return nil
	}
	return s.sharedFileWindows.prewarm(count, length, newNativeSharedFileWindow, "shared-file")
}

func (s *nativeFSKitServer) acquireSharedFileWindow(length int) (*nativeSharedReadWindow, bool, error) {
	return s.sharedFileWindows.acquire(length, newNativeSharedFileWindow)
}

func (s *nativeFSKitServer) recycleSharedFileWindow(window *nativeSharedReadWindow) bool {
	return s.sharedFileWindows.recycle(window)
}

func (s *nativeFSKitServer) closePrewarmedSharedFileWindows() error {
	return s.sharedFileWindows.close()
}

func (s *nativeFSKitServer) prewarmSharedMemoryWindows(count int, length int) error {
	if count == 0 || !nativeSharedReadFDAvailable() {
		return nil
	}
	return s.sharedMemoryWindows.prewarm(count, length, newNativeSharedReadWindow, "shared-memory")
}

func (s *nativeFSKitServer) acquireSharedMemoryWindow(length int) (*nativeSharedReadWindow, bool, error) {
	return s.sharedMemoryWindows.acquire(length, newNativeSharedReadWindow)
}

func (s *nativeFSKitServer) recycleSharedMemoryWindow(window *nativeSharedReadWindow) bool {
	return s.sharedMemoryWindows.recycle(window)
}

func (s *nativeFSKitServer) closePrewarmedSharedMemoryWindows() error {
	return s.sharedMemoryWindows.close()
}

type nativeSharedWindowFactory func(int) (*nativeSharedReadWindow, error)

func (p *nativeSharedFileWindowPool) prewarm(count int, length int, create nativeSharedWindowFactory, kind string) error {
	if length <= 0 || create == nil {
		return fmt.Errorf("invalid %s window prewarm request", kind)
	}
	windows := make([]*nativeSharedReadWindow, 0, count)
	for index := 0; index < count; index++ {
		window, err := create(length)
		if err != nil {
			var closeErr error
			for _, created := range windows {
				closeErr = errors.Join(closeErr, created.Close())
			}
			return errors.Join(fmt.Errorf("prewarm %s window %d: %w", kind, index, err), closeErr)
		}
		clear(window.mapping)
		windows = append(windows, window)
	}
	p.mu.Lock()
	p.limit = count
	p.windowBytes = length
	p.windows = append(p.windows, windows...)
	p.mu.Unlock()
	return nil
}

func (p *nativeSharedFileWindowPool) acquire(length int, create nativeSharedWindowFactory) (*nativeSharedReadWindow, bool, error) {
	p.mu.Lock()
	for index := len(p.windows) - 1; index >= 0; index-- {
		window := p.windows[index]
		if window.capacity != length {
			continue
		}
		p.windows = append(p.windows[:index], p.windows[index+1:]...)
		p.mu.Unlock()
		return window, true, nil
	}
	p.mu.Unlock()
	window, err := create(length)
	return window, false, err
}

func (p *nativeSharedFileWindowPool) recycle(window *nativeSharedReadWindow) bool {
	if window == nil {
		return false
	}
	p.mu.Lock()
	recycle := p.limit > 0 &&
		len(p.windows) < p.limit &&
		window.capacity == p.windowBytes &&
		window.file != nil &&
		window.mapping != nil
	if recycle {
		p.windows = append(p.windows, window)
	}
	p.mu.Unlock()
	if !recycle {
		_ = window.Close()
	}
	return recycle
}

func (p *nativeSharedFileWindowPool) close() error {
	p.mu.Lock()
	windows := p.windows
	p.windows = nil
	p.limit = 0
	p.windowBytes = 0
	p.mu.Unlock()
	var result error
	for _, window := range windows {
		result = errors.Join(result, window.Close())
	}
	return result
}

func (c *nativeFSKitConnection) dispatch(operation fskitproto.Op, payload []byte) ([]byte, int32) {
	c.server.nodes.syncVersion(c.server.filesystem.NamespaceVersion())
	decoder := fskitproto.NewDecoder(payload)
	switch operation {
	case fskitproto.OpPing:
		if err := decoder.Done(); err != nil {
			return nil, int32(syscall.EINVAL)
		}
		return nil, 0
	case fskitproto.OpGetattr:
		name, errno := decodePath(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		entry, errno := c.server.entry(name)
		if errno != 0 {
			return nil, int32(errno)
		}
		encoder := fskitproto.NewEncoder(160)
		encoder.EntryForCapabilities(entry, c.capabilities)
		return encoder.Data(), 0
	case fskitproto.OpReadDir:
		name, errno := decodePath(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		entries, errno := c.server.readDir(name)
		if errno != 0 {
			return nil, int32(errno)
		}
		encoder := fskitproto.NewEncoder(16 + len(entries)*160)
		encoder.Uint32(uint32(len(entries)))
		for _, entry := range entries {
			encoder.EntryForCapabilities(entry, c.capabilities)
		}
		return encoder.Data(), 0
	case fskitproto.OpOpen:
		name, flags, errno := decodeOpen(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		openFlags := int(flags &^ fskitproto.OpenFlagSnapshot)
		if cleanPath(name) == "/"+mountid.Path {
			if openFlags&(os.O_WRONLY|os.O_RDWR) != 0 {
				return nil, int32(syscall.EPERM)
			}
			handle := c.addHealthHandle(name, openFlags)
			encoder := fskitproto.NewEncoder(8)
			encoder.Uint64(handle)
			return encoder.Data(), 0
		}
		coreHandle, errno := c.server.filesystem.Open(name, openFlags)
		if errno != 0 {
			return nil, int32(errno)
		}
		handle := c.addHandle(coreHandle, name, openFlags, flags&fskitproto.OpenFlagSnapshot != 0)
		encoder := fskitproto.NewEncoder(8)
		encoder.Uint64(handle)
		return encoder.Data(), 0
	case fskitproto.OpCreate:
		name, flags, errno := decodeOpen(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		_, existing := c.server.filesystem.Getattr(name)
		if existing == 0 {
			return nil, int32(syscall.EEXIST)
		}
		if c.server.filesystem.nativeNamespaceRefreshActive(name) {
			return nil, int32(existing)
		}
		openFlags := int(flags&^fskitproto.OpenFlagSnapshot) | os.O_CREATE | os.O_EXCL
		coreHandle, errno := c.server.filesystem.Open(name, openFlags)
		if errno != 0 {
			return nil, int32(errno)
		}
		handle := c.addHandle(coreHandle, name, openFlags, flags&fskitproto.OpenFlagSnapshot != 0)
		c.server.filesystem.bumpNamespaceVersion()
		c.server.nodes.acceptVersion(c.server.filesystem.NamespaceVersion())
		entry, errno := c.server.entry(name)
		if errno != 0 {
			_ = c.server.filesystem.Release(coreHandle)
			delete(c.handles, handle)
			return nil, int32(errno)
		}
		encoder := fskitproto.NewEncoder(176)
		encoder.Uint64(handle)
		encoder.EntryForCapabilities(entry, c.capabilities)
		return encoder.Data(), 0
	case fskitproto.OpRead:
		handle, offset, length, errno := decodeRead(decoder, c.server.maxPayload)
		handleState, exists := c.handles[handle]
		if errno != 0 || !exists {
			if errno == 0 {
				errno = syscall.EBADF
			}
			return nil, int32(errno)
		}
		if handleState.health {
			if offset >= int64(len(c.server.health)) {
				encoder := fskitproto.NewEncoder(4)
				encoder.Bytes(nil)
				return encoder.Data(), 0
			}
			end := min(int64(len(c.server.health)), offset+int64(length))
			encoder := fskitproto.NewEncoder(4 + int(end-offset))
			encoder.Bytes(c.server.health[offset:end])
			return encoder.Data(), 0
		}
		buffer := make([]byte, length)
		started := time.Now()
		n, errno := c.server.filesystem.Read(handleState.coreHandle, buffer, offset)
		c.server.record(fmt.Sprintf("io=read_result handle=%d offset=%d bytes=%d duration_ns=%d", handle, offset, n, time.Since(started).Nanoseconds()))
		if errno != 0 {
			return nil, int32(errno)
		}
		encoder := fskitproto.NewEncoder(4 + n)
		encoder.Bytes(buffer[:n])
		return encoder.Data(), 0
	case fskitproto.OpWrite:
		handle, offset, data, errno := decodeWrite(decoder, c.server.maxPayload)
		c.server.record(fmt.Sprintf("io=write handle=%d offset=%d bytes=%d", handle, offset, len(data)))
		handleState, exists := c.handles[handle]
		if errno != 0 || !exists {
			if errno == 0 {
				errno = syscall.EBADF
			}
			return nil, int32(errno)
		}
		if handleState.health {
			return nil, int32(syscall.EROFS)
		}
		n, normalized, errno := c.writeHandle(handleState, data, offset)
		if errno != 0 {
			return nil, int32(errno)
		}
		if attribute, attrErrno := c.server.filesystem.Getattr(handleState.path); attrErrno == 0 {
			c.server.record(fmt.Sprintf("write_result handle=%d reported=%d normalized=%t visible=%d", handle, n, normalized, attribute.Size))
		}
		encoder := fskitproto.NewEncoder(4)
		encoder.Uint32(uint32(n))
		return encoder.Data(), 0
	case fskitproto.OpFsync, fskitproto.OpFlush, fskitproto.OpRelease:
		handle, err := decoder.Uint64()
		handleState, exists := c.handles[handle]
		if err != nil || decoder.Done() != nil || !exists {
			return nil, int32(syscall.EBADF)
		}
		if handleState.health {
			if operation == fskitproto.OpRelease {
				delete(c.handles, handle)
			}
			return nil, 0
		}
		var errno syscall.Errno
		switch operation {
		case fskitproto.OpFsync:
			errno = c.server.filesystem.Fsync(handleState.coreHandle)
		case fskitproto.OpFlush:
			errno = c.server.filesystem.Flush(handleState.coreHandle)
		case fskitproto.OpRelease:
			c.releaseSharedWindow(handleState)
			errno = c.server.filesystem.Release(handleState.coreHandle)
			delete(c.handles, handle)
		}
		return nil, int32(errno)
	case fskitproto.OpTruncate:
		name, errno := decodePathPrefix(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		size, err := decoder.Int64()
		if err != nil || size < 0 || decoder.Done() != nil {
			return nil, int32(syscall.EINVAL)
		}
		return nil, int32(c.server.filesystem.TruncatePath(name, size))
	case fskitproto.OpMkdir:
		name, errno := decodePathPrefix(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		mode, err := decoder.Uint32()
		if err != nil || decoder.Done() != nil {
			return nil, int32(syscall.EINVAL)
		}
		if c.server.filesystem.nativeNamespaceRefreshActive(name) {
			_, existing := c.server.filesystem.Getattr(name)
			if existing == 0 {
				return nil, int32(syscall.EEXIST)
			}
			return nil, int32(existing)
		}
		errno = c.server.filesystem.Mkdir(name, mode)
		if errno == 0 {
			c.server.nodes.acceptVersion(c.server.filesystem.NamespaceVersion())
		}
		return nil, int32(errno)
	case fskitproto.OpRename:
		oldName, errno := decodePathPrefix(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		newName, errno := decodePath(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		c.server.record(fmt.Sprintf("path_rename old=%q new=%q", oldName, newName))
		errno = c.server.filesystem.Rename(oldName, newName)
		if errno == 0 {
			c.server.nodes.rename(oldName, newName, c.server.filesystem.NamespaceVersion())
		}
		return nil, int32(errno)
	case fskitproto.OpUnlink, fskitproto.OpRmdir:
		name, errno := decodePath(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		if operation == fskitproto.OpUnlink {
			errno = c.server.filesystem.Unlink(name)
		} else {
			errno = c.server.filesystem.Rmdir(name)
		}
		if errno == 0 {
			c.server.nodes.remove(name, c.server.filesystem.NamespaceVersion())
		}
		return nil, int32(errno)
	case fskitproto.OpStatfs:
		if err := decoder.Done(); err != nil {
			return nil, int32(syscall.EINVAL)
		}
		stat, err := nativeFSKitStat(c.server.filesystem.nativeRoot)
		if err != nil {
			return nil, int32(errnoFor(err))
		}
		encoder := fskitproto.NewEncoder(64)
		encoder.StatFS(stat)
		return encoder.Data(), 0
	case fskitproto.OpSync:
		if err := decoder.Done(); err != nil {
			return nil, int32(syscall.EINVAL)
		}
		return nil, int32(c.server.filesystem.SyncAll())
	case fskitproto.OpNamespaceVersion:
		if err := decoder.Done(); err != nil {
			return nil, int32(syscall.EINVAL)
		}
		encoder := fskitproto.NewEncoder(8)
		encoder.Uint64(c.server.filesystem.NamespaceVersion())
		return encoder.Data(), 0
	case fskitproto.OpSetattr:
		name, errno := decodePathPrefix(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		valid, err := decoder.Uint32()
		if err != nil || valid&^(fskitproto.SetAttrMode|fskitproto.SetAttrUID|fskitproto.SetAttrGID|fskitproto.SetAttrAccessTime|fskitproto.SetAttrModifyTime) != 0 {
			return nil, int32(syscall.EINVAL)
		}
		mode, modeErr := decoder.Uint32()
		uid, uidErr := decoder.Uint32()
		gid, gidErr := decoder.Uint32()
		accessTime, accessErr := decoder.Time()
		modifyTime, modifyErr := decoder.Time()
		if errors.Join(modeErr, uidErr, gidErr, accessErr, modifyErr, decoder.Done()) != nil {
			return nil, int32(syscall.EINVAL)
		}
		request := SetAttrRequest{
			Valid: valid, Mode: mode, UID: uid, GID: gid,
			AccessTime: accessTime, ModTime: modifyTime,
		}
		return nil, int32(c.server.filesystem.SetAttributes(name, request))
	case fskitproto.OpGetXattr:
		name, errno := decodePathPrefix(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		attribute, err := decoder.String(4096)
		if err != nil || decoder.Done() != nil {
			return nil, int32(syscall.EINVAL)
		}
		value, errno := c.server.filesystem.GetXattr(name, attribute)
		if errno != 0 {
			return nil, int32(errno)
		}
		encoder := fskitproto.NewEncoder(4 + len(value))
		encoder.Bytes(value)
		return encoder.Data(), 0
	case fskitproto.OpSetXattr:
		name, errno := decodePathPrefix(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		attribute, err := decoder.String(4096)
		policy, policyErr := decoder.Uint32()
		value, valueErr := decoder.Bytes(int(c.server.maxPayload) - 8192)
		if errors.Join(err, policyErr, valueErr, decoder.Done()) != nil || policy > uint32(fskitproto.XattrDelete) {
			return nil, int32(syscall.EINVAL)
		}
		return nil, int32(c.server.filesystem.SetXattr(name, attribute, value, fskitproto.XattrPolicy(policy)))
	case fskitproto.OpListXattrs:
		name, errno := decodePath(decoder)
		if errno != 0 {
			return nil, int32(errno)
		}
		attributes, errno := c.server.filesystem.ListXattrs(name)
		if errno != 0 {
			return nil, int32(errno)
		}
		encoder := fskitproto.NewEncoder(4 + len(attributes)*64)
		encoder.Uint32(uint32(len(attributes)))
		for _, attribute := range attributes {
			encoder.String(attribute)
		}
		return encoder.Data(), 0
	default:
		return nil, int32(syscall.ENOSYS)
	}
}

func (c *nativeFSKitConnection) releaseHandles() {
	for _, handle := range c.handles {
		c.releaseSharedWindow(handle)
		if !handle.health {
			_ = c.server.filesystem.Release(handle.coreHandle)
		}
	}
}

func (c *nativeFSKitConnection) releaseSharedWindow(handle *nativeFSKitHandle) {
	window := handle.sharedWindow
	flag := handle.sharedWindowFlag
	handle.sharedWindow = nil
	handle.sharedWindowFlag = 0
	if window == nil {
		return
	}
	if flag == fskitproto.FlagSharedWindow && c.server.recycleSharedMemoryWindow(window) {
		return
	}
	if flag == fskitproto.FlagSharedFileWindow && c.server.recycleSharedFileWindow(window) {
		return
	}
	_ = window.Close()
}

func (c *nativeFSKitConnection) addHandle(coreHandle uint64, name string, flags int, snapshotWrite bool) uint64 {
	handle := c.nextHandle
	c.nextHandle++
	c.handles[handle] = &nativeFSKitHandle{
		coreHandle: coreHandle, path: name, flags: flags,
		snapshotWrite: snapshotWrite, snapshotFloor: -1,
	}
	return handle
}

func (c *nativeFSKitConnection) addHealthHandle(name string, flags int) uint64 {
	handle := c.nextHandle
	c.nextHandle++
	c.handles[handle] = &nativeFSKitHandle{path: name, flags: flags, health: true, snapshotFloor: -1}
	return handle
}

func (c *nativeFSKitConnection) writeHandle(handle *nativeFSKitHandle, data []byte, offset int64) (int, bool, syscall.Errno) {
	if !handle.snapshotWrite || len(data) == 0 {
		n, errno := c.server.filesystem.Write(handle.coreHandle, data, offset)
		return n, false, errno
	}
	currentPath, errno := c.server.filesystem.HandlePath(handle.coreHandle)
	if errno != 0 {
		return 0, false, errno
	}
	handle.path = currentPath
	attribute, errno := c.server.filesystem.Getattr(currentPath)
	if errno != 0 {
		return 0, false, errno
	}
	currentSize := attribute.Size
	if offset < 0 || offset > currentSize {
		return c.fallbackSnapshotWrite(handle, data, offset)
	}
	overlap := min(int64(len(data)), currentSize-offset)
	current := make([]byte, overlap)
	if overlap > 0 {
		n, readErrno := c.server.filesystem.Read(handle.coreHandle, current, offset)
		if readErrno != 0 {
			return 0, false, readErrno
		}
		current = current[:n]
	}
	common := commonPrefixBytes(current, data)
	if common == len(data) {
		return len(data), true, 0
	}
	if common == len(current) && offset+int64(common) == currentSize && completeJSONL(data[common:]) {
		floor := currentSize
		n, writeErrno := c.server.filesystem.Write(handle.coreHandle, data[common:], currentSize)
		if writeErrno == 0 {
			handle.snapshotFloor = floor
			return len(data), true, 0
		}
		return n, true, writeErrno
	}
	if handle.snapshotFloor >= offset && handle.snapshotFloor <= offset+int64(len(data)) {
		floorIndex := int(handle.snapshotFloor - offset)
		if floorIndex <= len(current) && bytes.Equal(data[:floorIndex], current[:floorIndex]) && completeJSONL(data[floorIndex:]) {
			n, writeErrno := c.server.filesystem.Write(handle.coreHandle, data[floorIndex:], currentSize)
			if writeErrno == 0 {
				return len(data), true, 0
			}
			return n, true, writeErrno
		}
	}
	return c.fallbackSnapshotWrite(handle, data, offset)
}

func (c *nativeFSKitConnection) fallbackSnapshotWrite(handle *nativeFSKitHandle, data []byte, offset int64) (int, bool, syscall.Errno) {
	if errno := c.server.filesystem.UseRandomWrites(handle.coreHandle); errno != 0 {
		return 0, false, errno
	}
	handle.flags &^= os.O_APPEND
	handle.snapshotWrite = false
	handle.snapshotFloor = -1
	n, writeErrno := c.server.filesystem.Write(handle.coreHandle, data, offset)
	return n, false, writeErrno
}

func commonPrefixBytes(left []byte, right []byte) int {
	limit := min(len(left), len(right))
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return limit
}

func (s *nativeFSKitServer) readDir(name string) ([]fskitproto.Entry, syscall.Errno) {
	names, errno := s.filesystem.ReadDir(name)
	if errno != 0 {
		return nil, errno
	}
	entries := make([]fskitproto.Entry, 0, len(names))
	for _, child := range names {
		entry, errno := s.entry(path.Join(name, child))
		if errno == syscall.ENOENT {
			continue
		}
		if errno != 0 {
			return nil, errno
		}
		entries = append(entries, entry)
	}
	if cleanPath(name) == "/" {
		entry, errno := s.entry("/" + mountid.Path)
		if errno != 0 {
			return nil, errno
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, 0
}

func (s *nativeFSKitServer) entry(name string) (fskitproto.Entry, syscall.Errno) {
	cleaned := cleanPath(name)
	if cleaned == "/"+mountid.Path {
		return fskitproto.Entry{
			Path: cleaned, Name: mountid.Path, NodeID: 3, ParentID: 2,
			Type: fskitproto.EntryFile, Mode: 0o400, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
			Size: uint64(len(s.health)), AllocSize: uint64((len(s.health) + 4095) &^ 4095),
			ModTime: s.startedAt, ChangeTime: s.startedAt, AccessTime: s.startedAt,
			NamespaceID: s.filesystem.NamespaceVersion(), ContentGeneration: 0,
		}, 0
	}
	attribute, errno := s.filesystem.Getattr(cleaned)
	if errno != 0 {
		if errno == syscall.ENOENT {
			s.nodes.forget(cleaned)
		}
		return fskitproto.Entry{}, errno
	}
	entryType := fskitproto.EntryUnknown
	switch attribute.Mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		entryType = fskitproto.EntryFile
	case syscall.S_IFDIR:
		entryType = fskitproto.EntryDirectory
	case syscall.S_IFLNK:
		entryType = fskitproto.EntrySymlink
	}
	nodeID := s.nodes.node(cleaned, attribute.ObjectID)
	parentID := uint64(1)
	if cleaned == "/" {
		parentID = 1
	} else {
		parentPath := path.Dir(cleaned)
		if parentAttribute, parentErrno := s.filesystem.Getattr(parentPath); parentErrno == 0 {
			parentID = s.nodes.node(parentPath, parentAttribute.ObjectID)
		} else {
			parentID = s.nodes.node(parentPath, "")
		}
	}
	allocated := uint64(0)
	if attribute.Size > 0 {
		allocated = uint64((attribute.Size + 4095) &^ 4095)
	}
	return fskitproto.Entry{
		Path: cleaned, Name: path.Base(cleaned), NodeID: nodeID, ParentID: parentID,
		Type: entryType, Mode: attribute.Mode & 0o7777, UID: attribute.UID, GID: attribute.GID,
		Size: uint64(max(attribute.Size, 0)), AllocSize: allocated,
		ModTime: attribute.ModTime, ChangeTime: attribute.ChangeTime, AccessTime: attribute.AccessTime,
		NamespaceID: s.filesystem.NamespaceVersion(), ContentGeneration: attribute.DirectoryGeneration,
	}, 0
}

func (s *nativeFSKitServer) record(message string) {
	if s.recorder != nil {
		s.recorder(message)
	}
}

func (n *nativeFSKitNodes) syncVersion(version uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.version = version
}

func (n *nativeFSKitNodes) acceptVersion(version uint64) {
	n.mu.Lock()
	n.version = version
	n.mu.Unlock()
}

func (n *nativeFSKitNodes) node(name string, objectID string) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if node, exists := n.byPath[name]; exists {
		if node.objectID == "" && objectID != "" {
			node.objectID = objectID
			n.byPath[name] = node
		}
		if objectID == "" || node.objectID == objectID {
			return node.id
		}
	}
	nodeID := n.next
	n.next++
	n.byPath[name] = nativeFSKitNode{id: nodeID, objectID: objectID}
	return nodeID
}

func (n *nativeFSKitNodes) forget(name string) {
	n.mu.Lock()
	delete(n.byPath, name)
	n.mu.Unlock()
}

func (n *nativeFSKitNodes) rename(oldName string, newName string, version uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	node, exists := n.byPath[oldName]
	if exists {
		delete(n.byPath, oldName)
		n.byPath[newName] = node
	}
	n.version = version
}

func (n *nativeFSKitNodes) remove(name string, version uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.byPath, name)
	n.version = version
}

func decodePath(decoder *fskitproto.Decoder) (string, syscall.Errno) {
	name, errno := decodePathPrefix(decoder)
	if errno != 0 {
		return "", errno
	}
	if decoder.Done() != nil {
		return "", syscall.EINVAL
	}
	return name, 0
}

func decodePathPrefix(decoder *fskitproto.Decoder) (string, syscall.Errno) {
	name, err := decoder.String(1 << 20)
	if err != nil || name == "" || cleanPath(name) != name {
		return "", syscall.EINVAL
	}
	return name, 0
}

func decodeOpen(decoder *fskitproto.Decoder) (string, uint32, syscall.Errno) {
	name, errno := decodePathPrefix(decoder)
	if errno != 0 {
		return "", 0, errno
	}
	flags, err := decoder.Uint32()
	if err != nil || decoder.Done() != nil {
		return "", 0, syscall.EINVAL
	}
	return name, flags, 0
}

func decodeRead(decoder *fskitproto.Decoder, maxPayload uint32) (uint64, int64, int, syscall.Errno) {
	handle, err := decoder.Uint64()
	if err != nil {
		return 0, 0, 0, syscall.EINVAL
	}
	offset, err := decoder.Int64()
	if err != nil || offset < 0 {
		return 0, 0, 0, syscall.EINVAL
	}
	length, err := decoder.Uint32()
	if err != nil || length > maxPayload-4 || decoder.Done() != nil {
		return 0, 0, 0, syscall.EINVAL
	}
	return handle, offset, int(length), 0
}

func decodeWrite(decoder *fskitproto.Decoder, maxPayload uint32) (uint64, int64, []byte, syscall.Errno) {
	handle, err := decoder.Uint64()
	if err != nil {
		return 0, 0, nil, syscall.EINVAL
	}
	offset, err := decoder.Int64()
	if err != nil || offset < 0 {
		return 0, 0, nil, syscall.EINVAL
	}
	data, err := decoder.Bytes(int(maxPayload) - 20)
	if err != nil || decoder.Done() != nil {
		return 0, 0, nil, syscall.EINVAL
	}
	return handle, offset, data, 0
}

func nativeFSKitOperationName(operation fskitproto.Op) string {
	switch operation {
	case fskitproto.OpHello:
		return "hello"
	case fskitproto.OpPing:
		return "ping"
	case fskitproto.OpGetattr:
		return "getattr"
	case fskitproto.OpReadDir:
		return "readdir"
	case fskitproto.OpOpen:
		return "open"
	case fskitproto.OpCreate:
		return "create"
	case fskitproto.OpRead:
		return "read"
	case fskitproto.OpWrite:
		return "write"
	case fskitproto.OpFsync:
		return "fsync"
	case fskitproto.OpFlush:
		return "flush"
	case fskitproto.OpRelease:
		return "release"
	case fskitproto.OpTruncate:
		return "truncate"
	case fskitproto.OpMkdir:
		return "mkdir"
	case fskitproto.OpRename:
		return "rename"
	case fskitproto.OpUnlink:
		return "unlink"
	case fskitproto.OpRmdir:
		return "rmdir"
	case fskitproto.OpStatfs:
		return "statfs"
	case fskitproto.OpSync:
		return "sync"
	case fskitproto.OpNamespaceVersion:
		return "namespace_version"
	case fskitproto.OpSetattr:
		return "setattr"
	case fskitproto.OpGetXattr:
		return "getxattr"
	case fskitproto.OpSetXattr:
		return "setxattr"
	case fskitproto.OpListXattrs:
		return "listxattrs"
	default:
		return "unknown"
	}
}
