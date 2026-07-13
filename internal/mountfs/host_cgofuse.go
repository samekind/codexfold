//go:build fuse && cgo

package mountfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/jstar0/codexfold/internal/mountid"
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/unix"
)

type fuseFilesystem struct {
	fuse.FileSystemBase
	core          *Filesystem
	recorder      func(string)
	mountIdentity []byte
}

const healthHandle = ^uint64(0) - 1

func Available() bool { return true }

func (f *fuseFilesystem) Getattr(name string, stat *fuse.Stat_t, _ uint64) int {
	f.record("getattr")
	if cleanPath(name) == "/"+mountid.Path {
		stat.Mode = syscall.S_IFREG | 0o400
		stat.Size = int64(len(f.mountIdentity))
		stat.Nlink = 1
		stat.Blksize = 4096
		stat.Blocks = (stat.Size + 511) / 512
		stat.Uid, stat.Gid, _ = fuse.Getcontext()
		return 0
	}
	attribute, errno := f.core.Getattr(name)
	if errno != 0 {
		return -int(errno)
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
	return 0
}

func (f *fuseFilesystem) Statfs(name string, stat *fuse.Statfs_t) int {
	f.core.mu.RLock()
	root := f.core.nativeRoot
	f.core.mu.RUnlock()
	if root == "" {
		result := -int(syscall.ENOENT)
		f.recordResult("statfs", name, result)
		return result
	}
	var source unix.Statfs_t
	result := unixResult(unix.Statfs(root, &source))
	if result == 0 {
		stat.Bsize = uint64(source.Bsize)
		if source.Iosize > 0 {
			stat.Frsize = uint64(source.Iosize)
		} else {
			stat.Frsize = uint64(source.Bsize)
		}
		stat.Blocks = source.Blocks
		stat.Bfree = source.Bfree
		stat.Bavail = source.Bavail
		stat.Files = source.Files
		stat.Ffree = source.Ffree
		stat.Favail = source.Ffree
		stat.Namemax = 255
	}
	f.recordResult("statfs", name, result)
	return result
}

func (f *fuseFilesystem) Mknod(name string, _ uint32, _ uint64) int {
	result := -int(syscall.ENOSYS)
	f.recordResult("mknod", name, result)
	return result
}

func (f *fuseFilesystem) Opendir(name string) (int, uint64) {
	f.record("opendir")
	if _, errno := f.core.ReadDir(name); errno != 0 {
		return -int(errno), ^uint64(0)
	}
	return 0, 0
}

func (f *fuseFilesystem) Readdir(name string, fill func(string, *fuse.Stat_t, int64) bool, _ int64, _ uint64) int {
	f.record("readdir")
	entries, errno := f.core.ReadDir(name)
	if errno != 0 {
		return -int(errno)
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
		if flags&fuse.O_ACCMODE != fuse.O_RDONLY {
			return -int(syscall.EPERM), ^uint64(0)
		}
		return 0, healthHandle
	}
	handle, errno := f.core.Open(name, translateOpenFlags(flags))
	if errno != 0 {
		result := -int(errno)
		f.recordResult("open", name, result)
		return result, ^uint64(0)
	}
	f.recordResult("open", name, 0)
	return 0, handle
}

func (f *fuseFilesystem) Create(name string, flags int, _ uint32) (int, uint64) {
	handle, errno := f.core.Open(name, translateOpenFlags(flags)|os.O_CREATE)
	if errno != 0 {
		result := -int(errno)
		f.recordResult("create", name, result)
		return result, ^uint64(0)
	}
	f.recordResult("create", name, 0)
	return 0, handle
}

func (f *fuseFilesystem) Read(_ string, destination []byte, offset int64, handle uint64) int {
	f.record("read")
	if handle == healthHandle {
		if offset < 0 || offset >= int64(len(f.mountIdentity)) {
			return 0
		}
		return copy(destination, f.mountIdentity[offset:])
	}
	n, errno := f.core.Read(handle, destination, offset)
	if errno != 0 {
		return -int(errno)
	}
	return n
}

func (f *fuseFilesystem) Write(_ string, data []byte, offset int64, handle uint64) int {
	f.record("write")
	n, errno := f.core.Write(handle, data, offset)
	if errno != 0 {
		return -int(errno)
	}
	return n
}

func (f *fuseFilesystem) Truncate(name string, size int64, handle uint64) int {
	f.record("truncate")
	var errno syscall.Errno
	if handle == 0 || handle == ^uint64(0) {
		errno = f.core.TruncatePath(name, size)
	} else {
		errno = f.core.Truncate(handle, size)
	}
	return -int(errno)
}

func (f *fuseFilesystem) Flush(_ string, handle uint64) int {
	f.record("flush")
	if handle == healthHandle {
		return 0
	}
	return -int(f.core.Flush(handle))
}

func (f *fuseFilesystem) Fsync(_ string, _ bool, handle uint64) int {
	f.record("fsync")
	if handle == healthHandle {
		return 0
	}
	return -int(f.core.Fsync(handle))
}

func (f *fuseFilesystem) Release(_ string, handle uint64) int {
	f.record("release")
	if handle == healthHandle {
		return 0
	}
	return -int(f.core.Release(handle))
}

func (f *fuseFilesystem) Mkdir(name string, mode uint32) int {
	result := -int(f.core.Mkdir(name, mode))
	f.recordResult("mkdir", name, result)
	return result
}

func (f *fuseFilesystem) Rmdir(name string) int {
	result := -int(syscall.ENOSYS)
	f.recordResult("rmdir", name, result)
	return result
}

func (f *fuseFilesystem) Link(oldName string, _ string) int {
	result := -int(syscall.ENOSYS)
	f.recordResult("link", oldName, result)
	return result
}

func (f *fuseFilesystem) Symlink(_ string, newName string) int {
	result := -int(syscall.ENOSYS)
	f.recordResult("symlink", newName, result)
	return result
}

func (f *fuseFilesystem) Readlink(name string) (int, string) {
	result := -int(syscall.ENOSYS)
	f.recordResult("readlink", name, result)
	return result, ""
}

func (f *fuseFilesystem) Rename(oldName string, newName string) int {
	result := -int(f.core.Rename(oldName, newName))
	f.recordResult("rename", oldName, result)
	return result
}

func (f *fuseFilesystem) Unlink(name string) int {
	result := -int(f.core.Unlink(name))
	f.recordResult("unlink", name, result)
	return result
}

func (f *fuseFilesystem) Access(name string, _ uint32) int {
	f.record("access")
	if cleanPath(name) == "/"+mountid.Path {
		return 0
	}
	_, errno := f.core.Getattr(name)
	return -int(errno)
}

func (f *fuseFilesystem) Chmod(name string, mode uint32) int {
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
	path, managed, errc := f.metadataPath(name)
	if errc != 0 {
		f.recordResult("utimens", name, errc)
		return errc
	}
	result := 0
	if !managed {
		if len(times) != 2 {
			result = -int(syscall.EINVAL)
		} else {
			unixTimes := []unix.Timespec{{Sec: times[0].Sec, Nsec: times[0].Nsec}, {Sec: times[1].Sec, Nsec: times[1].Nsec}}
			result = unixResult(unix.UtimesNanoAt(unix.AT_FDCWD, path, unixTimes, 0))
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
	return unixResult(unix.Setxattr(path, attribute, value, flags))
}

func (f *fuseFilesystem) Getxattr(name string, attribute string) (int, []byte) {
	f.record("getxattr")
	path, errc := f.xattrPath(name, false)
	if errc != 0 {
		return errc, nil
	}
	size, err := unix.Getxattr(path, attribute, nil)
	if err != nil {
		return unixResult(err), nil
	}
	value := make([]byte, size)
	n, err := unix.Getxattr(path, attribute, value)
	if err != nil {
		return unixResult(err), nil
	}
	return 0, value[:n]
}

func (f *fuseFilesystem) Listxattr(name string, fill func(string) bool) int {
	f.record("listxattr")
	path, errc := f.xattrPath(name, false)
	if errc != 0 {
		return errc
	}
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		return unixResult(err)
	}
	buffer := make([]byte, size)
	n, err := unix.Listxattr(path, buffer)
	if err != nil {
		return unixResult(err)
	}
	for _, attribute := range bytes.Split(bytes.TrimRight(buffer[:n], "\x00"), []byte{0}) {
		if len(attribute) != 0 && !fill(string(attribute)) {
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
	return unixResult(unix.Removexattr(path, attribute))
}

func (f *fuseFilesystem) xattrPath(name string, create bool) (string, int) {
	cleaned := cleanPath(name)
	if _, errno := f.core.sessionForPath(cleaned); errno == 0 {
		f.core.mu.RLock()
		root := f.core.nativeRoot
		f.core.mu.RUnlock()
		if root == "" {
			return "", -int(syscall.ENOTSUP)
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
	return "", -int(syscall.ENOENT)
}

func (f *fuseFilesystem) metadataPath(name string) (string, bool, int) {
	cleaned := cleanPath(name)
	if _, errno := f.core.sessionForPath(cleaned); errno == 0 {
		return "", true, 0
	}
	if native, ok := f.core.nativePath(cleaned); ok {
		return native, false, 0
	}
	return "", false, -int(syscall.ENOENT)
}

func unixResult(err error) int {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return -int(errno)
	}
	return -int(syscall.EIO)
}

func (f *fuseFilesystem) record(operation string) {
	if f.recorder != nil {
		f.recorder(operation)
	}
}

func (f *fuseFilesystem) recordResult(operation string, name string, result int) {
	kind := "other"
	base := filepath.Base(name)
	if strings.HasPrefix(base, "._") {
		kind = "appledouble"
	} else if strings.HasSuffix(base, ".jsonl") {
		kind = "session"
	}
	f.record(fmt.Sprintf("%s kind=%s result=%d", operation, kind, result))
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
	return translated
}

func mountHost(ctx context.Context, options HostOptions) (result error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = fmt.Errorf("%w: %v", ErrPrerequisite, recovered)
		}
	}()
	identity, err := mountid.New()
	if err != nil {
		return fmt.Errorf("generate mount identity: %w", err)
	}
	filesystem := &fuseFilesystem{core: options.Filesystem, recorder: options.OperationRecorder, mountIdentity: []byte(identity)}
	host := fuse.NewFileSystemHost(filesystem)
	arguments := []string{"-o", "fsname=codexfold", "-o", "default_permissions", "-o", "attr_timeout=0", "-o", "entry_timeout=0", "-o", "negative_timeout=0"}
	if options.Foreground {
		arguments = append(arguments, "-f")
	}
	if runtime.GOOS == "darwin" {
		arguments = append(arguments, "-o", "volname=CodexFold")
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = host.Unmount()
		case <-done:
		}
	}()
	mounted := host.Mount(options.MountPoint, arguments)
	close(done)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !mounted {
		return errors.New("FUSE host exited without mounting")
	}
	return ctx.Err()
}
