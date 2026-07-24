package fold

import (
	"context"
	"io"
)

// ObjectReader opens a verified stream for one content-addressed object.
type ObjectReader interface {
	OpenObject(context.Context, ObjectRef) (io.ReadCloser, error)
	HasObject(ObjectRef) bool
}

type fallbackObjectReader struct {
	primary  ObjectReader
	fallback ObjectReader
}

func (r fallbackObjectReader) OpenObject(ctx context.Context, ref ObjectRef) (io.ReadCloser, error) {
	if r.primary.HasObject(ref) {
		return r.primary.OpenObject(ctx, ref)
	}
	return r.fallback.OpenObject(ctx, ref)
}

func (r fallbackObjectReader) HasObject(ref ObjectRef) bool {
	return r.primary.HasObject(ref) || r.fallback.HasObject(ref)
}

func openObjectReader(storeDir string, reader ObjectReader) ObjectReader {
	if reader != nil {
		return reader
	}
	return NewObjectStore(storeDir)
}
