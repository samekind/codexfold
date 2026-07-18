//go:build !darwin

package mountfs

import (
	"os"

	"github.com/jstar0/codexfold/internal/fskitproto"
)

func nativeFSKitStat(string) (fskitproto.StatFS, error) {
	return fskitproto.StatFS{
		BlockSize: 4096, IOSize: 4 * 1024 * 1024,
		TotalBytes: 1 << 40, AvailableBytes: 1 << 39, FreeBytes: 1 << 39, UsedBytes: 1 << 39,
		TotalFiles: 1 << 32, FreeFiles: 1 << 31,
	}, nil
}

var _ = os.ErrNotExist
