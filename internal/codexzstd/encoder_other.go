//go:build !cgo || !(darwin || linux)

package codexzstd

import "io"

func Supported() bool { return false }

type encoder struct{}

func newEncoder(io.Writer, int64) (*encoder, error) { return nil, ErrUnsupported }

func (*encoder) Write([]byte) (int, error) { return 0, ErrUnsupported }

func (*encoder) Close() error { return ErrUnsupported }

func (*encoder) abort() {}
