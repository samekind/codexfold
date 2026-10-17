package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

func startNamespaceRetirement(ctx context.Context, filesystem *mountfs.Filesystem, store string, enabled func() bool, diagnostics io.Writer) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		previous := ""
		for {
			err := filesystem.RetireReplacedFiles(ctx)
			if err == nil && enabled() {
				err = filesystem.FoldNativeReplacements(ctx, func(ctx context.Context, source, id string) (*vfs.Session, io.Closer, error) {
					return prepareNamespaceReplacement(ctx, store, source, id)
				})
			}
			if errors.Is(err, storage.ErrOperationLockHeld) || errors.Is(err, storage.ErrManagedSessionBusy) {
				err = nil
			}
			if err != nil && !errors.Is(err, context.Canceled) && err.Error() != previous {
				fmt.Fprintf(diagnostics, "replaced file cleanup: %v\n", err)
				previous = err.Error()
			}
			if err == nil {
				previous = ""
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

func prepareNamespaceReplacement(ctx context.Context, store, source, id string) (session *vfs.Session, closer io.Closer, err error) {
	maintenance, err := storage.BeginMaintenance(ctx, store)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		err = maintenance.YieldError(err)
		maintenance.Close()
	}()
	ctx = maintenance.Context()
	resolver, err := pack.Open(store, pack.OpenOptions{})
	if err != nil {
		return nil, nil, err
	}
	result, err := fold.Fold(ctx, fold.Session{ID: id, RolloutPath: source}, fold.FoldOptions{StoreDir: store, Apply: true, Opaque: true, ExistingReader: resolver})
	if err != nil {
		resolver.Close()
		return nil, nil, err
	}
	snapshot, err := retainCanonicalSnapshot(ctx, store, id, vfs.NativeFile{Path: source, Bytes: result.SourceBytes, SHA256: result.SourceSHA256}, nil)
	if err != nil {
		resolver.Close()
		return nil, nil, err
	}
	manifest, err := fold.LoadManifestPath(result.ManifestPath)
	if err != nil {
		resolver.Close()
		return nil, nil, err
	}
	session, err = vfs.OpenSession(ctx, vfs.SessionOptions{Root: store, ManifestPath: result.ManifestPath, Manifest: manifest, Reader: mountfs.NamespaceReader(store, resolver), NativeSnapshot: snapshot})
	if err != nil {
		resolver.Close()
		return nil, nil, err
	}
	return session, resolver, nil
}
