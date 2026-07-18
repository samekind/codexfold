//go:build !darwin

package mountfs

import "context"

func (f *Filesystem) WatchNativeNamespace(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
