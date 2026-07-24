package fold

import (
	"context"
	"io"
)

// ObjectReader opens a verified stream for one content-addressed object.
type ObjectReader interface {
	OpenObject(context.Context, ObjectRef) (io.ReadCloser, error)
}

func openObjectReader(storeDir string, reader ObjectReader) ObjectReader {
	if reader != nil {
		return reader
	}
	return NewObjectStore(storeDir)
}
