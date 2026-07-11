//go:build !fuse || !cgo

package mountfs

import "context"

func mountHost(context.Context, HostOptions) error { return ErrPrerequisite }
