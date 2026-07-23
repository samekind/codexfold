//go:build !darwin && !linux

package mountfs

import (
	"syscall"

	"github.com/samekind/codexfold/internal/fskitproto"
)

func platformSetXattr(string, string, []byte, fskitproto.XattrPolicy) error { return syscall.ENOTSUP }
func platformGetXattr(string, string) ([]byte, error)                       { return nil, syscall.ENOTSUP }
func platformListXattrs(string) ([]string, error)                           { return nil, syscall.ENOTSUP }
func platformRemoveXattr(string, string) error                              { return syscall.ENOTSUP }
func xattrMissingErrno() syscall.Errno                                      { return syscall.ENOENT }
