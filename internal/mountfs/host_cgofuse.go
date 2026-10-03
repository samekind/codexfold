//go:build (darwin && fuse && cgo) || (linux && fuse && fuse3 && cgo) || (windows && winfsp)

package mountfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/samekind/codexfold/internal/buildid"
	"github.com/samekind/codexfold/internal/mountid"
	"github.com/winfsp/cgofuse/fuse"
)

type fuseFilesystem struct {
	fuse.FileSystemBase
	core           *Filesystem
	backend        HostBackend
	backendBuild   func() string
	backendHealthy func() bool
	recorder       func(string)
	mountIdentity  []byte
	statRoot       string
	mountReady     atomic.Bool
	activity       *IOActivityCounter
}

const healthHandle = ^uint64(0) - 1

func (f *fuseFilesystem) operations() HostBackend {
	if f.backend != nil {
		return f.backend
	}
	return f.core
}

func (f *fuseFilesystem) healthBytes() []byte {
	if f.backendBuild == nil {
		return f.mountIdentity
	}
	sha := f.backendBuild()
	if !buildid.ValidSHA256(sha) {
		return nil
	}
	index := strings.LastIndexByte(string(f.mountIdentity), ':')
	return []byte(string(f.mountIdentity[:index+1]) + sha)
}

func (f *fuseFilesystem) healthy() bool {
	return f.mountReady.Load() && (f.backendHealthy == nil || f.backendHealthy())
}

func Available() bool { return true }

func (f *fuseFilesystem) Getattr(name string, stat *fuse.Stat_t, _ uint64) int {
	if cleanPath(name) == "/"+mountid.Path {
		if !f.healthy() {
			f.recordResult("getattr", name, fuseResult(syscall.ENOENT))
			return fuseResult(syscall.ENOENT)
		}
		stat.Mode = syscall.S_IFREG | 0o400
		stat.Size = int64(len(f.healthBytes()))
		stat.Nlink = 1
		stat.Blksize = 4096
		stat.Blocks = (stat.Size + 511) / 512
		stat.Uid, stat.Gid, _ = fuse.Getcontext()
		f.recordResult("getattr", name, 0)
		return 0
	}
	attribute, errno := f.operations().Getattr(name)
	if errno != 0 {
		f.recordResult("getattr", name, fuseResult(errno))
		return fuseResult(errno)
	}
	stat.Mode = attribute.Mode
	stat.Size = attribute.Size
	stat.Nlink = 1
	stat.Blksize = 4096
	stat.Blocks = (attribute.Size + 511) / 512
	stat.Mtim = fuse.NewTimespec(attribute.ModTime)
	stat.Ctim = stat.Mtim
	stat.Atim = stat.Mtim
	stat.Uid, stat.Gid, _ = fuse.Getcontext()
	f.recordResult("getattr", name, 0)
	return 0
}

func (f *fuseFilesystem) Statfs(name string, stat *fuse.Statfs_t) int {
	root := ""
	if f.core != nil {
		f.core.mu.RLock()
		root = f.core.nativeRoot
		f.core.mu.RUnlock()
	}
	if root == "" {
		root = f.statRoot
	}
	result := populateFilesystemStat(root, stat)
	f.recordResult("statfs", name, result)
	return result
}

func (f *fuseFilesystem) Mknod(name string, _ uint32, _ uint64) int {
	result := fuseResult(syscall.ENOSYS)
	f.recordResult("mknod", name, result)
	return result
}

func (f *fuseFilesystem) Opendir(name string) (int, uint64) {
	f.record("opendir")
	if _, errno := f.operations().ReadDir(name); errno != 0 {
		return fuseResult(errno), ^uint64(0)
	}
	return 0, 0
}

func (f *fuseFilesystem) Readdir(name string, fill func(string, *fuse.Stat_t, int64) bool, _ int64, _ uint64) int {
	f.record("readdir")
	entries, errno := f.operations().ReadDir(name)
	if errno != 0 {
		return fuseResult(errno)
	}
	fill(".", nil, 0)
	fill("..", nil, 0)
	for _, entry := range entries {
		if !fill(entry, nil, 0) {
			break
		}
	}
	return 0
}

func (f *fuseFilesystem) Open(name string, flags int) (int, uint64) {
	if cleanPath(name) == "/"+mountid.Path {
		if !f.healthy() {
			return fuseResult(syscall.ENOENT), ^uint64(0)
		}
		if flags&fuse.O_ACCMODE != fuse.O_RDONLY {
			return fuseResult(syscall.EPERM), ^uint64(0)
		}
		return 0, healthHandle
	}
	translated := translateOpenFlags(flags)
	handle, errno := f.operations().Open(name, translated)
	if errno == syscall.EBUSY && writableSession(name, flags) {
		retryFor := 250 * time.Millisecond
		if runtime.GOOS == "windows" {
			// Recovery promotes newly discovered read-only sessions on
			// the background loader's one-second poll.
			retryFor = 1500 * time.Millisecond
		}
		deadline := time.Now().Add(retryFor)
		for errno == syscall.EBUSY && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			handle, errno = f.operations().Open(name, translated)
		}
	}
	if errno != 0 {
		result := fuseResult(errno)
		f.recordOpen("open", name, flags, translated, handle, result)
		return result, ^uint64(0)
	}
	f.recordOpen("open", name, flags, translated, handle, 0)
	return 0, handle
}

func (f *fuseFilesystem) Create(name string, flags int, _ uint32) (int, uint64) {
	translated := translateOpenFlags(flags) | os.O_CREATE
	handle, errno := f.operations().Open(name, translated)
	if errno != 0 {
		result := fuseResult(errno)
		f.recordOpen("create", name, flags, translated, handle, result)
		return result, ^uint64(0)
	}
	f.recordOpen("create", name, flags, translated, handle, 0)
	return 0, handle
}

func (f *fuseFilesystem) OpenEx(name string, info *fuse.FileInfo_t) int {
	result, handle := f.Open(name, info.Flags)
	if result == 0 {
		info.Fh = handle
		info.DirectIo = writableSession(name, info.Flags)
		f.record(fmt.Sprintf("open_config kind=%s handle=%d direct_io=%t", operationKind(name), handle, info.DirectIo))
	}
	return result
}

func (f *fuseFilesystem) CreateEx(name string, _ uint32, info *fuse.FileInfo_t) int {
	result, handle := f.Create(name, info.Flags, 0o600)
	if result == 0 {
		info.Fh = handle
		info.DirectIo = writableSession(name, info.Flags)
		f.record(fmt.Sprintf("create_config kind=%s handle=%d direct_io=%t", operationKind(name), handle, info.DirectIo))
	}
	return result
}

func (f *fuseFilesystem) Read(name string, destination []byte, offset int64, handle uint64) int {
	if handle == healthHandle {
		identity := f.healthBytes()
		if offset < 0 || offset >= int64(len(identity)) {
			f.recordIO("read", name, handle, offset, len(destination), 0)
			return 0
		}
		n := copy(destination, identity[offset:])
		f.recordIO("read", name, handle, offset, len(destination), n)
		return n
	}
	n, errno := f.operations().Read(handle, destination, offset)
	if errno != 0 {
		result := fuseResult(errno)
		f.recordIO("read", name, handle, offset, len(destination), result)
		return result
	}
	f.recordIO("read", name, handle, offset, len(destination), n)
	f.activity.recordRead(n)
	return n
}

func (f *fuseFilesystem) Write(name string, data []byte, offset int64, handle uint64) int {
	n, errno := f.operations().Write(handle, data, offset)
	if errno != 0 {
		result := fuseResult(errno)
		f.recordIO("write", name, handle, offset, len(data), result)
		return result
	}
	f.recordIO("write", name, handle, offset, len(data), n)
	f.activity.recordWrite(n)
	return n
}

func (f *fuseFilesystem) Truncate(name string, size int64, handle uint64) int {
	var errno syscall.Errno
	if handle == 0 || handle == ^uint64(0) {
		errno = f.operations().TruncatePath(name, size)
	} else {
		errno = f.operations().Truncate(handle, size)
	}
	result := fuseResult(errno)
	f.record(fmt.Sprintf("truncate kind=%s handle=%d size=%d result=%d", operationKind(name), handle, size, result))
	return result
}

func (f *fuseFilesystem) Flush(name string, handle uint64) int {
	if handle == healthHandle {
		f.recordHandleResult("flush", name, handle, 0)
		return 0
	}
	result := fuseResult(f.operations().Flush(handle))
	f.recordHandleResult("flush", name, handle, result)
	return result
}

func (f *fuseFilesystem) Fsync(name string, dataOnly bool, handle uint64) int {
	if handle == healthHandle {
		f.record(fmt.Sprintf("fsync kind=%s handle=%d datasync=%t result=0", operationKind(name), handle, dataOnly))
		return 0
	}
	result := fuseResult(f.operations().Fsync(handle))
	f.record(fmt.Sprintf("fsync kind=%s handle=%d datasync=%t result=%d", operationKind(name), handle, dataOnly, result))
	return result
}

func (f *fuseFilesystem) Release(name string, handle uint64) int {
	if handle == healthHandle {
		f.recordHandleResult("release", name, handle, 0)
		return 0
	}
	result := fuseResult(f.operations().Release(handle))
	f.recordHandleResult("release", name, handle, result)
	return result
}

func (f *fuseFilesystem) Mkdir(name string, mode uint32) int {
	result := fuseResult(f.operations().Mkdir(name, mode))
	f.recordResult("mkdir", name, result)
	return result
}

func (f *fuseFilesystem) Rmdir(name string) int {
	result := fuseResult(syscall.ENOSYS)
	f.recordResult("rmdir", name, result)
	return result
}

func (f *fuseFilesystem) Link(oldName string, _ string) int {
	result := fuseResult(syscall.ENOSYS)
	f.recordResult("link", oldName, result)
	return result
}

func (f *fuseFilesystem) Symlink(_ string, newName string) int {
	result := fuseResult(syscall.ENOSYS)
	f.recordResult("symlink", newName, result)
	return result
}

func (f *fuseFilesystem) Readlink(name string) (int, string) {
	result := fuseResult(syscall.ENOSYS)
	f.recordResult("readlink", name, result)
	return result, ""
}

func (f *fuseFilesystem) Rename(oldName string, newName string) int {
	result := fuseResult(f.operations().Rename(oldName, newName))
	f.recordResult("rename", oldName, result)
	return result
}

func (f *fuseFilesystem) Unlink(name string) int {
	result := fuseResult(f.operations().Unlink(name))
	f.recordResult("unlink", name, result)
	return result
}

func (f *fuseFilesystem) Access(name string, _ uint32) int {
	f.record("access")
	if cleanPath(name) == "/"+mountid.Path {
		return 0
	}
	_, errno := f.operations().Getattr(name)
	return fuseResult(errno)
}

func (f *fuseFilesystem) Chmod(name string, mode uint32) int {
	if remote, ok := f.backend.(HostMetadataBackend); ok {
		return fuseResult(remote.Metadata("chmod", name, mode, 0, time.Time{}, time.Time{}))
	}
	path, managed, errc := f.metadataPath(name)
	if errc != 0 {
		f.recordResult("chmod", name, errc)
		return errc
	}
	result := 0
	if !managed {
		result = unixResult(os.Chmod(path, os.FileMode(mode)&os.ModePerm))
	}
	f.recordResult("chmod", name, result)
	return result
}

func (f *fuseFilesystem) Chown(name string, uid uint32, gid uint32) int {
	if remote, ok := f.backend.(HostMetadataBackend); ok {
		return fuseResult(remote.Metadata("chown", name, uid, gid, time.Time{}, time.Time{}))
	}
	path, managed, errc := f.metadataPath(name)
	if errc != 0 {
		f.recordResult("chown", name, errc)
		return errc
	}
	result := 0
	if !managed {
		result = unixResult(os.Chown(path, int(uid), int(gid)))
	}
	f.recordResult("chown", name, result)
	return result
}

func (f *fuseFilesystem) Utimens(name string, times []fuse.Timespec) int {
	if remote, ok := f.backend.(HostMetadataBackend); ok {
		if len(times) != 2 {
			return fuseResult(syscall.EINVAL)
		}
		return fuseResult(remote.Metadata("utimens", name, 0, 0,
			time.Unix(times[0].Sec, times[0].Nsec), time.Unix(times[1].Sec, times[1].Nsec)))
	}
	path, managed, errc := f.metadataPath(name)
	if errc != 0 {
		f.recordResult("utimens", name, errc)
		return errc
	}
	result := 0
	if !managed {
		if len(times) != 2 {
			result = fuseResult(syscall.EINVAL)
		} else {
			result = setFileTimes(path, times)
		}
	}
	f.recordResult("utimens", name, result)
	return result
}

func (f *fuseFilesystem) Setxattr(name string, attribute string, value []byte, flags int) int {
	f.record("setxattr")
	path, errc := f.xattrPath(name, true)
	if errc != 0 {
		return errc
	}
	return setExtendedAttribute(path, attribute, value, flags)
}

func (f *fuseFilesystem) Getxattr(name string, attribute string) (int, []byte) {
	f.record("getxattr")
	path, errc := f.xattrPath(name, false)
	if errc != 0 {
		return errc, nil
	}
	return getExtendedAttribute(path, attribute)
}

func (f *fuseFilesystem) Listxattr(name string, fill func(string) bool) int {
	f.record("listxattr")
	path, errc := f.xattrPath(name, false)
	if errc != 0 {
		return errc
	}
	result, attributes := listExtendedAttributes(path)
	if result != 0 {
		return result
	}
	for _, attribute := range attributes {
		if attribute != "" && !fill(attribute) {
			break
		}
	}
	return 0
}

func (f *fuseFilesystem) Removexattr(name string, attribute string) int {
	f.record("removexattr")
	path, errc := f.xattrPath(name, false)
	if errc != 0 {
		return errc
	}
	return removeExtendedAttribute(path, attribute)
}

func (f *fuseFilesystem) xattrPath(name string, create bool) (string, int) {
	if f.core == nil {
		return "", fuseResult(syscall.ENOSYS)
	}
	cleaned := cleanPath(name)
	if _, _, errno := f.core.sessionForPath(cleaned); errno == 0 {
		f.core.mu.RLock()
		root := f.core.nativeRoot
		f.core.mu.RUnlock()
		if root == "" {
			return "", fuseResult(syscall.ENOTSUP)
		}
		carrier := managedXattrCarrier(root, cleaned)
		if create {
			if err := os.MkdirAll(filepath.Dir(carrier), 0o700); err != nil {
				return "", unixResult(err)
			}
			file, err := os.OpenFile(carrier, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				return "", unixResult(err)
			}
			if err := file.Close(); err != nil {
				return "", unixResult(err)
			}
		}
		return carrier, 0
	}
	if native, ok := f.core.nativePath(cleaned); ok {
		return native, 0
	}
	return "", fuseResult(syscall.ENOENT)
}

func (f *fuseFilesystem) metadataPath(name string) (string, bool, int) {
	cleaned := cleanPath(name)
	if _, _, errno := f.core.sessionForPath(cleaned); errno == 0 {
		return "", true, 0
	}
	if native, ok := f.core.nativePath(cleaned); ok {
		return native, false, 0
	}
	return "", false, fuseResult(syscall.ENOENT)
}

func unixResult(err error) int {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fuseResult(errno)
	}
	return fuseResult(syscall.EIO)
}

func (f *fuseFilesystem) record(operation string) {
	if f.recorder != nil {
		f.recorder(operation)
	}
}

func (f *fuseFilesystem) recordResult(operation string, name string, result int) {
	f.record(fmt.Sprintf("%s kind=%s result=%d", operation, operationKind(name), result))
}

func (f *fuseFilesystem) recordOpen(operation string, name string, flags int, translated int, handle uint64, result int) {
	f.record(fmt.Sprintf("%s kind=%s flags=%#x translated=%#x handle=%d result=%d", operation, operationKind(name), flags, translated, handle, result))
}

func (f *fuseFilesystem) recordIO(operation string, name string, handle uint64, offset int64, bytes int, result int) {
	f.record(fmt.Sprintf("%s kind=%s handle=%d offset=%d bytes=%d result=%d", operation, operationKind(name), handle, offset, bytes, result))
}

func (f *fuseFilesystem) recordHandleResult(operation string, name string, handle uint64, result int) {
	f.record(fmt.Sprintf("%s kind=%s handle=%d result=%d", operation, operationKind(name), handle, result))
}

func operationKind(name string) string {
	kind := "other"
	base := filepath.Base(name)
	if strings.HasPrefix(base, "._") {
		kind = "appledouble"
	} else if strings.HasSuffix(base, ".jsonl") {
		kind = "session"
	}
	return kind
}

func writableSession(name string, flags int) bool {
	return operationKind(name) == "session" && flags&fuse.O_ACCMODE != fuse.O_RDONLY
}

func translateOpenFlags(flags int) int {
	translated := os.O_RDONLY
	switch flags & fuse.O_ACCMODE {
	case fuse.O_WRONLY:
		translated = os.O_WRONLY
	case fuse.O_RDWR:
		translated = os.O_RDWR
	}
	if flags&fuse.O_APPEND != 0 {
		translated |= os.O_APPEND
	}
	if flags&fuse.O_TRUNC != 0 {
		translated |= os.O_TRUNC
	}
	if flags&fuse.O_CREAT != 0 {
		translated |= os.O_CREATE
	}
	if flags&fuse.O_EXCL != 0 {
		translated |= os.O_EXCL
	}
	if runtime.GOOS == "windows" && translated&os.O_WRONLY != 0 {
		// WinFsp may read boundary blocks through a write handle while
		// preparing an append. Windows access checks have already approved
		// the caller; the backing handle needs both capabilities.
		translated = translated&^os.O_WRONLY | os.O_RDWR
	}
	return translated
}

func mountHost(ctx context.Context, options HostOptions) (result error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = fmt.Errorf("%w: %v", ErrPrerequisite, recovered)
		}
	}()
	if err := validateFuseProvider(); err != nil {
		return fmt.Errorf("validate selected FUSE provider: %w", err)
	}
	buildSHA256 := options.BuildSHA256
	var err error
	if buildSHA256 == "" {
		buildSHA256, err = buildid.CurrentSHA256()
		if err != nil {
			return fmt.Errorf("hash mounted executable: %w", err)
		}
	}
	identity, err := mountid.New(buildSHA256)
	if err != nil {
		return fmt.Errorf("generate mount identity: %w", err)
	}
	statRoot := options.StorageRoot
	if statRoot == "" {
		statRoot = filepath.Dir(options.MountPoint)
		if runtime.GOOS == "windows" && statRoot == options.MountPoint {
			return errors.New("Windows drive mounts require a physical storage root")
		}
	}
	filesystem := &fuseFilesystem{
		core:           options.Filesystem,
		backend:        options.Backend,
		backendBuild:   options.BackendBuild,
		backendHealthy: options.BackendHealthy,
		recorder:       options.OperationRecorder,
		mountIdentity:  []byte(identity),
		statRoot:       statRoot,
		activity:       options.Activity,
	}
	host := fuse.NewFileSystemHost(filesystem)
	backing, err := prepareMountedBacking(options.MountPoint)
	if err != nil {
		return fmt.Errorf("prepare mount backing permissions: %w", err)
	}
	backingClosed := false
	defer func() {
		if !backingClosed {
			_ = backing.Close()
		}
	}()
	arguments := []string{"-o", "fsname=codexfold", "-o", "default_permissions", "-o", "attr_timeout=0", "-o", "entry_timeout=0", "-o", "negative_timeout=0"}
	securityArguments, err := platformMountSecurity(options.NamespaceRoot)
	if err != nil {
		return err
	}
	arguments = append(arguments, securityArguments...)
	if options.Foreground {
		arguments = append(arguments, "-f")
	}
	if runtime.GOOS == "darwin" {
		arguments = append(arguments, "-o", "backend=nfs", "-o", "volname=CodexFold")
	}
	mountTarget := platformMountTarget(options.MountPoint)
	if runtime.GOOS == "windows" && filepath.Dir(options.MountPoint) == options.MountPoint {
		arguments = append(arguments, "-o", "volname=CodexFold")
		if !strings.HasPrefix(mountTarget, `\\.\`) {
			// Foreground previews use a network drive so ordinary clients can
			// canonicalize its paths without administrator privileges. SCM
			// uses a Mount Manager disk drive, which also supports the NTFS
			// junctions in the canonical session namespace.
			arguments = append(arguments, "-o", "VolumePrefix="+windowsVolumePrefix(options.NamespaceRoot, options.MountPoint))
		}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = host.Unmount()
		case <-done:
		}
	}()
	policyContext, cancelPolicy := context.WithCancel(ctx)
	policyDone := make(chan error, 1)
	go func() {
		err := configureMountedFilesystem(policyContext, options.MountPoint)
		if err == nil {
			err = backing.Seal()
		}
		if err == nil {
			filesystem.mountReady.Store(true)
		} else {
			_ = host.Unmount()
		}
		policyDone <- err
	}()
	mounted := host.Mount(mountTarget, arguments)
	cancelPolicy()
	policyErr := <-policyDone
	backingErr := backing.Close()
	backingClosed = true
	close(done)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !mounted {
		return errors.New("FUSE host exited without mounting")
	}
	if policyErr != nil {
		return fmt.Errorf("configure mounted filesystem: %w", policyErr)
	}
	if backingErr != nil {
		return fmt.Errorf("seal unmounted backing directory: %w", backingErr)
	}
	return ctx.Err()
}
