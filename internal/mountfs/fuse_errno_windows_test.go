//go:build windows && winfsp

package mountfs

import (
	"syscall"
	"testing"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

func TestWindowsFuseErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code syscall.Errno
		want int
	}{
		{0, 0}, {syscall.ENOENT, -fuse.ENOENT}, {syscall.EBUSY, -fuse.EBUSY},
		{syscall.ENOSYS, -fuse.ENOSYS}, {syscall.ENOTSUP, -fuse.ENOTSUP},
		{windows.ERROR_ACCESS_DENIED, -fuse.EACCES}, {syscall.ENOTDIR, -fuse.ENOTDIR},
		{windows.ERROR_SHARING_VIOLATION, -fuse.EBUSY}, {windows.ERROR_DISK_FULL, -fuse.ENOSPC},
	} {
		if got := fuseResult(tc.code); got != tc.want {
			t.Errorf("%v: got %d want %d", tc.code, got, tc.want)
		}
	}
	fs := &fuseFilesystem{core: New()}
	if got, _ := fs.Open("/missing.jsonl", fuse.O_RDONLY); got != -fuse.ENOENT {
		t.Fatalf("missing file must reach WinFsp as ENOENT: %d", got)
	}
}
