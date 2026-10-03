//go:build windows

package mountfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/samekind/codexfold/internal/buildid"
	"golang.org/x/sys/windows"
)

type WindowsCoreOptions struct {
	Binary, Root, Store string
	Arguments           []string
	Stdout, Stderr      io.Writer
	BeforeUpdate        func() error
	ReadyTimeout        time.Duration
}
type coreProcess struct {
	command              *exec.Cmd
	control              io.WriteCloser
	client               *rpc.Client
	token, binary, build string
	done                 chan struct{}
	err                  error
}
type WindowsCoreHost struct {
	gate      sync.RWMutex
	handlesMu sync.Mutex
	handles   map[uint64]bool
	process   *coreProcess
	options   WindowsCoreOptions
	healthy   atomic.Bool
	build     atomic.Value
	closed    bool
}
type CoreInfo struct {
	Version, HostPID, EnginePID, OpenHandles int
	Binary, Build                            string
	Healthy                                  bool
}
type CoreUpdateRequest struct {
	Candidate string
	Apply     bool
}
type CoreUpdateResult struct {
	CoreInfo
	CandidateSHA256                 string
	PreviousPID, ReplacementPID     int
	Changed, DryRun, MountPreserved bool
	RecoveryBinary                  string
}
type coreImage struct {
	Version int
	SHA256  string
}

func WindowsCoreControlPipe(store string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(store))))
	return `\\.\pipe\codexfold-core-control-` + hex.EncodeToString(digest[:16])
}

func NewWindowsCoreHost(ctx context.Context, options WindowsCoreOptions) (*WindowsCoreHost, error) {
	for _, path := range []string{options.Binary, options.Root, options.Store} {
		if !filepath.IsAbs(path) {
			return nil, errors.New("absolute engine binary, image root and store are required")
		}
	}
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = 2 * time.Minute
	}
	if err := os.MkdirAll(options.Root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(options.Root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("engine image root must be an ordinary directory")
	}
	binary := options.Binary
	data, err := os.ReadFile(filepath.Join(options.Root, "current.json"))
	if err == nil {
		var image coreImage
		if err := json.Unmarshal(data, &image); err != nil || image.Version != WindowsCoreProtocol || !buildid.ValidSHA256(image.SHA256) {
			return nil, errors.New("persisted engine binding is invalid")
		}
		binary = filepath.Join(options.Root, image.SHA256+".exe")
		if info, err := os.Lstat(binary); err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("persisted engine image is not an ordinary file")
		}
		if actual, err := buildid.FileSHA256(binary); err != nil || actual != image.SHA256 {
			return nil, errors.New("persisted engine image failed verification")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	host := &WindowsCoreHost{options: options, handles: map[uint64]bool{}}
	process, err := host.start(ctx, binary)
	if err != nil {
		return nil, err
	}
	host.process = process
	host.build.Store(process.build)
	host.healthy.Store(true)
	return host, nil
}

func (host *WindowsCoreHost) start(ctx context.Context, binary string) (*coreProcess, error) {
	build, err := buildid.FileSHA256(binary)
	if err != nil {
		return nil, err
	}
	boot := WindowsCoreBoot{Pipe: `\\.\pipe\codexfold-core-` + rand.Text(), Token: rand.Text() + rand.Text()}
	command := exec.Command(binary, host.options.Arguments...)
	command.Stdout, command.Stderr = host.options.Stdout, host.options.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	control, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		control.Close()
		return nil, err
	}
	process := &coreProcess{command: command, control: control, token: boot.Token, binary: binary, build: build, done: make(chan struct{})}
	go func() { process.err = command.Wait(); close(process.done) }()
	if err := json.NewEncoder(control).Encode(boot); err != nil {
		_ = host.stop(process)
		return nil, err
	}
	ready, cancel := context.WithTimeout(ctx, host.options.ReadyTimeout)
	defer cancel()
	for {
		connection, err := winio.DialPipeContext(ready, boot.Pipe)
		if err == nil {
			client := rpc.NewClient(connection)
			var reply CoreResponse
			call := client.Go("Core.Execute", CoreRequest{Token: boot.Token, Operation: "hello"}, &reply, make(chan *rpc.Call, 1))
			select {
			case result := <-call.Done:
				if result.Error == nil && reply.Version == WindowsCoreProtocol && reply.PID == command.Process.Pid && reply.Build == build {
					process.client = client
					return process, nil
				}
			case <-ready.Done():
			}
			_ = client.Close()
		}
		select {
		case <-process.done:
			_ = control.Close()
			return nil, fmt.Errorf("engine exited before becoming ready: %w", process.err)
		case <-ready.Done():
			_ = host.stop(process)
			return nil, fmt.Errorf("engine readiness: %w", ready.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (host *WindowsCoreHost) stop(process *coreProcess) error {
	if process == nil {
		return nil
	}
	_ = process.control.Close()
	select {
	case <-process.done:
		if process.client != nil {
			_ = process.client.Close()
		}
		return process.err
	case <-time.After(25 * time.Second):
		// A graceful shutdown that failed is never treated as a successful
		// cutover. Kill only this owned child, then return an explicit failure.
		_ = process.command.Process.Kill()
		<-process.done
		if process.client != nil {
			_ = process.client.Close()
		}
		return errors.New("engine did not finish its graceful shutdown")
	}
}

func (host *WindowsCoreHost) BuildSHA256() string {
	value, _ := host.build.Load().(string)
	return value
}
func (host *WindowsCoreHost) Healthy() bool {
	if !host.healthy.Load() {
		return false
	}
	host.gate.RLock()
	defer host.gate.RUnlock()
	if host.process == nil {
		return false
	}
	select {
	case <-host.process.done:
		return false
	default:
		return true
	}
}

func (host *WindowsCoreHost) infoLocked() CoreInfo {
	host.handlesMu.Lock()
	handles := len(host.handles)
	host.handlesMu.Unlock()
	info := CoreInfo{Version: WindowsCoreProtocol, HostPID: os.Getpid(), OpenHandles: handles, Build: host.BuildSHA256(), Healthy: host.healthy.Load()}
	if host.process != nil {
		info.EnginePID = host.process.command.Process.Pid
		info.Binary = host.process.binary
	}
	return info
}

func (host *WindowsCoreHost) Update(ctx context.Context, request CoreUpdateRequest) (CoreUpdateResult, error) {
	if !filepath.IsAbs(request.Candidate) {
		return CoreUpdateResult{}, errors.New("absolute candidate path is required")
	}
	info, err := os.Lstat(request.Candidate)
	if err != nil || !info.Mode().IsRegular() {
		return CoreUpdateResult{}, errors.New("candidate must be an ordinary executable file")
	}
	candidateSHA, err := buildid.FileSHA256(request.Candidate)
	if err != nil {
		return CoreUpdateResult{}, err
	}
	host.gate.Lock()
	defer host.gate.Unlock()
	result := CoreUpdateResult{CoreInfo: host.infoLocked(), CandidateSHA256: candidateSHA, DryRun: !request.Apply, Changed: candidateSHA != host.BuildSHA256()}
	if host.closed {
		return result, errors.New("resident mount is stopping")
	}
	if !result.Changed {
		result.MountPreserved = true
		return result, nil
	}
	if result.OpenHandles != 0 {
		return result, fmt.Errorf("live update waits for %d open file handles to close", result.OpenHandles)
	}
	if host.options.BeforeUpdate != nil {
		if err := host.options.BeforeUpdate(); err != nil {
			return result, err
		}
	}
	if !request.Apply {
		return result, nil
	}
	image, err := host.stage(request.Candidate, candidateSHA)
	if err != nil {
		return result, err
	}
	previous := host.process
	result.PreviousPID, result.RecoveryBinary = previous.command.Process.Pid, previous.binary
	host.healthy.Store(false)
	if err := host.stop(previous); err != nil {
		return result, fmt.Errorf("old engine did not stop cleanly; candidate was not started: %w", err)
	}
	cutover, cancelCutover := context.WithTimeout(ctx, 45*time.Second)
	replacement, updateErr := host.start(cutover, image)
	cancelCutover()
	if updateErr == nil {
		updateErr = host.persist(candidateSHA)
	}
	if updateErr != nil {
		if replacement != nil {
			_ = host.stop(replacement)
		}
		rollback, rollbackErr := host.start(ctx, previous.binary)
		if rollbackErr == nil {
			host.process = rollback
			host.build.Store(rollback.build)
			host.healthy.Store(true)
		}
		return result, errors.Join(fmt.Errorf("candidate failed; previous engine restored when possible: %w", updateErr), rollbackErr)
	}
	host.process = replacement
	host.build.Store(replacement.build)
	host.healthy.Store(true)
	result.CoreInfo = host.infoLocked()
	result.ReplacementPID, result.MountPreserved = replacement.command.Process.Pid, true
	return result, nil
}

func (host *WindowsCoreHost) stage(candidate, sha string) (string, error) {
	image := filepath.Join(host.options.Root, sha+".exe")
	if existing, err := os.Lstat(image); err == nil {
		if !existing.Mode().IsRegular() {
			return "", errors.New("engine image is not an ordinary file")
		}
		if actual, err := buildid.FileSHA256(image); err != nil || actual != sha {
			return "", errors.New("existing engine image failed verification")
		}
		return image, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	source, err := os.Open(candidate)
	if err != nil {
		return "", err
	}
	defer source.Close()
	temporary, err := os.CreateTemp(host.options.Root, ".engine-*.tmp")
	if err != nil {
		return "", err
	}
	name := temporary.Name()
	defer os.Remove(name)
	_, copyErr := io.Copy(temporary, source)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return "", err
	}
	if actual, err := buildid.FileSHA256(name); err != nil || actual != sha {
		return "", errors.New("candidate changed while being staged")
	}
	if err := os.Rename(name, image); err != nil {
		return "", err
	}
	return image, nil
}

func (host *WindowsCoreHost) persist(sha string) error {
	data, err := json.Marshal(coreImage{Version: WindowsCoreProtocol, SHA256: sha})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(host.options.Root, ".current-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	from, _ := windows.UTF16PtrFromString(name)
	to, _ := windows.UTF16PtrFromString(filepath.Join(host.options.Root, "current.json"))
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func (host *WindowsCoreHost) Close() error {
	host.gate.Lock()
	defer host.gate.Unlock()
	if host.closed {
		return nil
	}
	host.closed = true
	host.healthy.Store(false)
	return host.stop(host.process)
}

type coreControlServer struct {
	host *WindowsCoreHost
	ctx  context.Context
}

func (control *coreControlServer) Info(_ struct{}, reply *CoreInfo) error {
	control.host.gate.RLock()
	defer control.host.gate.RUnlock()
	*reply = control.host.infoLocked()
	return nil
}
func (control *coreControlServer) Update(request CoreUpdateRequest, reply *CoreUpdateResult) error {
	result, err := control.host.Update(control.ctx, request)
	*reply = result
	return err
}

func (host *WindowsCoreHost) ServeControl(ctx context.Context) (func(), error) {
	security, err := corePipeSecurity()
	if err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(WindowsCoreControlPipe(host.options.Store), &winio.PipeConfig{SecurityDescriptor: security})
	if err != nil {
		return nil, err
	}
	server := rpc.NewServer()
	if err := server.RegisterName("Control", &coreControlServer{host: host, ctx: ctx}); err != nil {
		listener.Close()
		return nil, err
	}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(4 * time.Minute))
				server.ServeConn(connection)
			}()
		}
	}()
	return func() { _ = listener.Close() }, nil
}

func CallWindowsCoreControl(ctx context.Context, store, method string, argument, reply any) error {
	connection, err := winio.DialPipeContext(ctx, WindowsCoreControlPipe(store))
	if err != nil {
		return fmt.Errorf("resident Windows engine control is unavailable; this mount may require one offline upgrade: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	client := rpc.NewClient(connection)
	defer client.Close()
	call := client.Go("Control."+method, argument, reply, make(chan *rpc.Call, 1))
	select {
	case result := <-call.Done:
		return result.Error
	case <-ctx.Done():
		return ctx.Err()
	}
}
