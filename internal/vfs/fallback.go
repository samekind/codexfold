package vfs

import (
	"context"
)

func (s *Session) CreateCurrentNativeBacking(ctx context.Context, target string) (NativeFile, error) {
	return s.MaterializeCurrent(ctx, target, false)
}
