//go:build !darwin && fuse && cgo

package mountfs

import "context"

func configureMountedFilesystem(context.Context, string) error { return nil }
