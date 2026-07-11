//go:build !fuse || !cgo

package mountfs

import "context"

func Available() bool { return false }

func mountHost(context.Context, HostOptions) error { return ErrPrerequisite }
