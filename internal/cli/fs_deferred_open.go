package cli

import (
	"context"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/vfs"
)

// openManagedSessionDeferred is the owned-loader path. It may serve an exact
// published session, but leaves every recovery and repair action to reload.
func openManagedSessionDeferred(ctx context.Context, store string, state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
	manifest, err := fold.LoadManifestPath(state.ManifestPath)
	if err != nil {
		return nil, nil, err
	}
	resolver, err := pack.Open(store, pack.OpenOptions{})
	if err != nil {
		return nil, nil, err
	}
	managed, err := vfs.OpenSession(ctx, vfs.SessionOptions{
		Root: store, ManifestPath: state.ManifestPath, Manifest: manifest, Reader: resolver,
		NativeSnapshot: state.NativeSnapshot, DeferRecovery: true,
	})
	if err != nil {
		_ = resolver.Close()
		return nil, nil, err
	}
	return managed, resolver, nil
}
