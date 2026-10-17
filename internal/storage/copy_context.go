package storage

import (
	"context"
	"io"
)

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(buffer)
}

// CopyContext checks cancellation between bounded reads, including while a
// single multi-gigabyte pack file is being hashed during maintenance.
func CopyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	return io.CopyBuffer(destination, contextReader{ctx: ctx, Reader: source}, make([]byte, 64<<10))
}
