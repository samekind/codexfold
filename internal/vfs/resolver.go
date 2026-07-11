package vfs

import (
	"context"

	"github.com/jstar0/codexfold/internal/fold"
)

type ObjectReader interface {
	ReadAt(context.Context, fold.ObjectRef, []byte, int64) (int, error)
}
