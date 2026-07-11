//go:build fuse && cgo

package mountfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/winfsp/cgofuse/fuse"
)

type fuseFilesystem struct {
	fuse.FileSystemBase
	core *Filesystem
}

func Available() bool { return true }

func (f *fuseFilesystem) Getattr(name string, stat *fuse.Stat_t, _ uint64) int {
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

func (f *fuseFilesystem) Opendir(name string) (int, uint64) {
	if _, errno := f.core.ReadDir(name); errno != 0 {
		return -int(errno), ^uint64(0)
	}
	return 0, 0
}

func (f *fuseFilesystem) Readdir(name string, fill func(string, *fuse.Stat_t, int64) bool, _ int64, _ uint64) int {
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
	handle, errno := f.core.Open(name, translateOpenFlags(flags))
	if errno != 0 {
		return -int(errno), ^uint64(0)
	}
	return 0, handle
}

func (f *fuseFilesystem) Read(_ string, destination []byte, offset int64, handle uint64) int {
	n, errno := f.core.Read(handle, destination, offset)
	if errno != 0 {
		return -int(errno)
	}
	return n
}

func (f *fuseFilesystem) Write(_ string, data []byte, offset int64, handle uint64) int {
	n, errno := f.core.Write(handle, data, offset)
	if errno != 0 {
		return -int(errno)
	}
	return n
}

func (f *fuseFilesystem) Truncate(name string, size int64, handle uint64) int {
	var errno syscall.Errno
	if handle == 0 || handle == ^uint64(0) {
		errno = f.core.TruncatePath(name, size)
	} else {
		errno = f.core.Truncate(handle, size)
	}
	return -int(errno)
}

func (f *fuseFilesystem) Flush(_ string, handle uint64) int {
	return -int(f.core.Flush(handle))
}

func (f *fuseFilesystem) Fsync(_ string, _ bool, handle uint64) int {
	return -int(f.core.Fsync(handle))
}

func (f *fuseFilesystem) Release(_ string, handle uint64) int {
	return -int(f.core.Release(handle))
}

func (f *fuseFilesystem) Rename(oldName string, newName string) int {
	return -int(f.core.Rename(oldName, newName))
}

func (f *fuseFilesystem) Unlink(name string) int { return -int(f.core.Unlink(name)) }

func (f *fuseFilesystem) Access(name string, _ uint32) int {
	_, errno := f.core.Getattr(name)
	return -int(errno)
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
	return translated
}

func mountHost(ctx context.Context, options HostOptions) (result error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = fmt.Errorf("%w: %v", ErrPrerequisite, recovered)
		}
	}()
	filesystem := &fuseFilesystem{core: options.Filesystem}
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
	if !mounted {
		return errors.New("FUSE host exited without mounting")
	}
	return ctx.Err()
}
