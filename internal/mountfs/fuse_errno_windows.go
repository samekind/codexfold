//go:build windows && winfsp

package mountfs

import (
	"syscall"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

// Go's Windows syscall package uses synthetic numbers for POSIX errors.
// WinFsp's FUSE boundary expects the errno constants exported by cgofuse.
func fuseResult(errno syscall.Errno) int {
	switch errno {
	case 0:
		return 0
	case syscall.EPERM:
		return -fuse.EPERM
	case syscall.ENOENT:
		return -fuse.ENOENT
	case syscall.EACCES, windows.ERROR_ACCESS_DENIED, windows.ERROR_PRIVILEGE_NOT_HELD:
		return -fuse.EACCES
	case syscall.EBUSY, windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION:
		return -fuse.EBUSY
	case syscall.EEXIST, windows.ERROR_ALREADY_EXISTS, windows.ERROR_FILE_EXISTS:
		return -fuse.EEXIST
	case syscall.EINVAL, windows.ERROR_INVALID_PARAMETER:
		return -fuse.EINVAL
	case syscall.ENOTDIR, windows.ERROR_DIRECTORY:
		return -fuse.ENOTDIR
	case syscall.EISDIR:
		return -fuse.EISDIR
	case syscall.ENOTEMPTY, windows.ERROR_DIR_NOT_EMPTY:
		return -fuse.ENOTEMPTY
	case syscall.ENOSPC, windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL:
		return -fuse.ENOSPC
	case syscall.EBADF, windows.ERROR_INVALID_HANDLE:
		return -fuse.EBADF
	case syscall.EROFS, windows.ERROR_WRITE_PROTECT:
		return -fuse.EROFS
	case syscall.ENOSYS, windows.ERROR_CALL_NOT_IMPLEMENTED:
		return -fuse.ENOSYS
	case syscall.ENOTSUP, syscall.EOPNOTSUPP, windows.ERROR_NOT_SUPPORTED:
		return -fuse.ENOTSUP
	case syscall.EINTR:
		return -fuse.EINTR
	case syscall.EAGAIN:
		return -fuse.EAGAIN
	case syscall.EFBIG:
		return -fuse.EFBIG
	case syscall.ENAMETOOLONG, windows.ERROR_FILENAME_EXCED_RANGE:
		return -fuse.ENAMETOOLONG
	case syscall.EMFILE, windows.ERROR_TOO_MANY_OPEN_FILES:
		return -fuse.EMFILE
	default:
		return -fuse.EIO
	}
}
