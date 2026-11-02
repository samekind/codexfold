package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
)

// A dry-run fold prices a session exactly: it reads and compresses every
// object but writes nothing. Prices are cached by file identity and pack
// generation, since only those change what a fold would add.

type enrollmentPrice struct {
	Path            string `json:"path"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	Generation      string `json:"generation"`
	NewBytes        int64  `json:"new_bytes"`
}

var enrollmentPriceMu sync.Mutex

func enrollmentPricePath(store string) string {
	return filepath.Join(filepath.Clean(store), "enrollment", "prices.json")
}

func enrollmentDryRunPricer(store string) func(context.Context, codex.Session, os.FileInfo) (int64, error) {
	return func(ctx context.Context, session codex.Session, info os.FileInfo) (int64, error) {
		generation, _ := pack.CurrentGeneration(store)
		enrollmentPriceMu.Lock()
		defer enrollmentPriceMu.Unlock()
		prices := make(map[string]enrollmentPrice)
		if data, err := os.ReadFile(enrollmentPricePath(store)); err == nil {
			_ = json.Unmarshal(data, &prices)
		}
		if cached, ok := prices[session.ID]; ok && cached.Path == session.RolloutPath && cached.Size == info.Size() &&
			cached.ModTimeUnixNano == info.ModTime().UnixNano() && cached.Generation == generation {
			return cached.NewBytes, nil
		}
		newBytes, err := dryRunFoldBytes(ctx, store, toFoldSession(session), fold.FoldOptions{})
		if err != nil {
			return 0, err
		}
		prices[session.ID] = enrollmentPrice{
			Path: session.RolloutPath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(),
			Generation: generation, NewBytes: newBytes,
		}
		data, err := json.Marshal(prices)
		if err == nil {
			_ = writeFileAtomically(enrollmentPricePath(store), data)
		}
		return newBytes, nil
	}
}

// dryRunFoldBytes reports what folding session would add to store.
func dryRunFoldBytes(ctx context.Context, store string, session fold.Session, options fold.FoldOptions) (int64, error) {
	options.StoreDir = store
	options.Apply, options.Overwrite, options.RemoveSource = false, false, false
	if options.ExistingReader == nil {
		if resolver, err := pack.Open(store, pack.OpenOptions{CacheBytes: -1}); err == nil {
			defer resolver.Close()
			options.ExistingReader = resolver
		}
	}
	result, err := fold.Fold(ctx, session, options)
	if err != nil {
		return 0, err
	}
	return result.NewStoredBytes, nil
}

// foldSpaceIsTight reports whether pricing a fold at its full source size
// would leave less than a comfortable margin above the free-space reserve.
var foldSpaceIsTight = func(store string, sourceBytes int64) bool {
	limits, err := storage.LoadLimits(store)
	if err != nil {
		return false
	}
	available, err := storage.AvailableBytes(store)
	if err != nil {
		return false
	}
	return available < limits.FreeSpaceReserveBytes+sourceBytes+sourceBytes/16+(2<<30)
}

func writeFileAtomically(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".prices-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return errors.Join(os.Rename(name, path))
}
