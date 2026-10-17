//go:build windows

package mountfs

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/rpc"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/samekind/codexfold/internal/buildid"
	"golang.org/x/sys/windows"
)

const WindowsCoreProtocol = 1
const coreMaxIO = 8 << 20

type WindowsCoreBoot struct{ Pipe, Token string }
type CoreRequest struct {
	Token, Operation, Name, Target string
	Handle                         uint64
	Flags, Length                  int
	Offset, Size                   int64
	Mode, UID                      uint32
	AccessTime, ModTime            time.Time
	Data                           []byte
}
type CoreResponse struct {
	Errno               syscall.Errno
	Attr                Attr
	Entries             []string
	Handle              uint64
	Count, Version, PID int
	Build               string
	Data                []byte
}

func corePipeSecurity() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	// Reject network logons even if they belong to the local Administrators
	// group. The SYSTEM host exposes control only to SYSTEM/administrators.
	return "D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + user.User.Sid.String() + ")", nil
}

func ReadWindowsCoreBoot(reader io.Reader) (WindowsCoreBoot, error) {
	var boot WindowsCoreBoot
	if err := json.NewDecoder(io.LimitReader(reader, 4096)).Decode(&boot); err != nil {
		return boot, err
	}
	if !strings.HasPrefix(boot.Pipe, `\\.\pipe\codexfold-core-`) || len(boot.Token) < 32 || len(boot.Token) > 128 {
		return boot, errors.New("invalid private engine launch binding")
	}
	return boot, nil
}

type coreRPCServer struct {
	fs           *Filesystem
	token, build string
}

func (server *coreRPCServer) Execute(request CoreRequest, reply *CoreResponse) error {
	if subtle.ConstantTimeCompare([]byte(request.Token), []byte(server.token)) != 1 {
		return errors.New("engine authentication failed")
	}
	f := server.fs
	switch request.Operation {
	case "hello":
		reply.Version, reply.PID, reply.Build = WindowsCoreProtocol, os.Getpid(), server.build
	case "getattr":
		reply.Attr, reply.Errno = f.Getattr(request.Name)
	case "readdir":
		reply.Entries, reply.Errno = f.ReadDir(request.Name)
	case "open":
		reply.Handle, reply.Errno = f.Open(request.Name, request.Flags)
	case "read":
		if request.Length < 0 || request.Length > coreMaxIO {
			reply.Errno = syscall.EINVAL
			break
		}
		reply.Data = make([]byte, request.Length)
		reply.Count, reply.Errno = f.Read(request.Handle, reply.Data, request.Offset)
		reply.Data = reply.Data[:reply.Count]
	case "write":
		if len(request.Data) > coreMaxIO {
			reply.Errno = syscall.EINVAL
			break
		}
		reply.Count, reply.Errno = f.Write(request.Handle, request.Data, request.Offset)
	case "truncate":
		reply.Errno = f.Truncate(request.Handle, request.Size)
	case "truncate-path":
		reply.Errno = f.TruncatePath(request.Name, request.Size)
	case "flush":
		reply.Errno = f.Flush(request.Handle)
	case "fsync":
		reply.Errno = f.Fsync(request.Handle)
	case "release":
		reply.Errno = f.Release(request.Handle)
	case "mkdir":
		reply.Errno = f.Mkdir(request.Name, request.Mode)
	case "rename":
		reply.Errno = f.Rename(request.Name, request.Target)
	case "unlink":
		reply.Errno = f.Unlink(request.Name)
	case "chmod", "chown", "utimens":
		path, managed, errno := f.metadataPath(request.Name)
		reply.Errno = errno
		if errno != 0 || managed {
			break
		}
		var err error
		switch request.Operation {
		case "chmod":
			err = os.Chmod(path, os.FileMode(request.Mode)&os.ModePerm)
		case "chown":
			err = os.Chown(path, int(request.Mode), int(request.UID))
		case "utimens":
			err = os.Chtimes(path, request.AccessTime, request.ModTime)
		}
		reply.Errno = errnoFor(err)
	default:
		reply.Errno = syscall.ENOSYS
	}
	return nil
}

// ServeWindowsCore reuses the existing storage engine and its recovery logic.
// Closing the parent lifeline cancels this context; RPC calls finish before the
// caller closes session owners and releases the exclusive service lock.
func ServeWindowsCore(ctx context.Context, fs *Filesystem, boot WindowsCoreBoot, onReady func()) error {
	security, err := corePipeSecurity()
	if err != nil {
		return err
	}
	listener, err := winio.ListenPipe(boot.Pipe, &winio.PipeConfig{SecurityDescriptor: security, InputBufferSize: 65536, OutputBufferSize: 65536})
	if err != nil {
		return err
	}
	defer listener.Close()
	build, err := buildid.CurrentSHA256()
	if err != nil {
		return err
	}
	server := rpc.NewServer()
	if err := server.RegisterName("Core", &coreRPCServer{fs: fs, token: boot.Token, build: build}); err != nil {
		return err
	}
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var calls sync.WaitGroup
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		_ = listener.Close()
		mu.Lock()
		for connection := range connections {
			_ = connection.Close()
		}
		mu.Unlock()
	}()
	if onReady != nil {
		onReady()
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			calls.Wait()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		mu.Lock()
		connections[connection] = true
		mu.Unlock()
		calls.Add(1)
		go func() {
			defer calls.Done()
			defer connection.Close()
			server.ServeConn(connection)
			mu.Lock()
			delete(connections, connection)
			mu.Unlock()
		}()
	}
}
