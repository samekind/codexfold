//go:build !windows

package cli

import (
	"context"
	"errors"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/spf13/cobra"
)

func prepareWindowsStorageEngine(*cobra.Command) (func(), error) {
	return nil, errors.New("Windows engine unavailable")
}
func serveWindowsStorageEngine(context.Context, *cobra.Command, *mountfs.Filesystem, func()) error {
	return errors.New("Windows engine unavailable")
}
func serveWindowsResidentHost(*cobra.Command, string, string, string, bool) error {
	return errors.New("Windows engine unavailable")
}
