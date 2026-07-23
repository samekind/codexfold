package vfs

import (
	"context"

	"github.com/samekind/codexfold/internal/fold"
)

type ObjectReader interface {
	ReadAt(context.Context, fold.ObjectRef, []byte, int64) (int, error)
}
