//go:build !windows

package cli

import (
	"context"
	"github.com/samekind/codexfold/internal/mountfs"
	"io"
)

func startPlatformDaemonStatus(context.Context, string, string, *mountfs.IOActivityCounter, io.Writer) func() {
	return func() {}
}
