package mountfs

import (
	"context"
	"errors"
)

var ErrPrerequisite = errors.New("FUSE host prerequisite is unavailable in this build")

type HostOptions struct {
	MountPoint string
	Filesystem *Filesystem
	Foreground bool
}

func Mount(ctx context.Context, options HostOptions) error {
	if options.MountPoint == "" || options.Filesystem == nil {
		return errors.New("mount point and filesystem are required")
	}
	return mountHost(ctx, options)
}
