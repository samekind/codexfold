//go:build (!darwin && !linux && !windows) || (darwin && (!fuse || !cgo)) || (linux && (!fuse || !fuse3 || !cgo)) || (windows && !winfsp)

package mountfs

import "context"

func Available() bool { return false }

func mountHost(context.Context, HostOptions) error { return ErrPrerequisite }
