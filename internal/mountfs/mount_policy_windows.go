//go:build windows && winfsp

package mountfs

import "context"

func configureMountedFilesystem(context.Context, string) error { return nil }
