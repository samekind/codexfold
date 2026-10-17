//go:build (darwin && fuse && cgo) || (linux && fuse && fuse3 && cgo)

package mountfs

import "syscall"

func fuseResult(errno syscall.Errno) int { return -int(errno) }
