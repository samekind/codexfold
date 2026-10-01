package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

type nativeRetirementBatchKey struct{}
type nativeRetirementWorkersKey struct{}
type nativeRetirementIdleKey struct{}

func nativeRetirementInventory(ctx context.Context, store string) (storage.Inventory, error) {
	if _, ok := ctx.Value(nativeRetirementBatchKey{}).(*nativeRetirementBatch); ok {
		return storage.Inventory{}, nil
	}
	return storage.Scan(ctx, storage.Options{StoreDir: store, AllowMetadataIssues: true})
}

func nativeRetirementState(ctx context.Context, store, id string) (vfs.SessionState, error) {
	if _, ok := ctx.Value(nativeRetirementBatchKey{}).(*nativeRetirementBatch); !ok {
		return managedState(store, id)
	}
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return vfs.SessionState{}, errors.New("invalid retirement session identity")
	}
	state, err := vfs.InspectSessionState(filepath.Join(store, "fs", "sessions", id, "state.json"))
	if err == nil && state.SessionID != id {
		return state, errors.New("retirement state belongs to another session")
	}
	return state, err
}

func runNativeRetirementGroup(ctx context.Context, states []vfs.SessionState, run func(vfs.SessionState) error, report func(vfs.SessionState, error)) {
	workers, _ := ctx.Value(nativeRetirementWorkersKey{}).(int)
	if workers < 2 {
		for _, state := range states {
			if ctx.Err() != nil {
				return
			}
			report(state, run(state))
		}
		return
	}
	workers = min(workers, 16)
	type outcome struct {
		state vfs.SessionState
		err   error
	}
	results := make(chan outcome, workers)
	idle, _ := ctx.Value(nativeRetirementIdleKey{}).(func() bool)
	next, active := 0, 0
	for next < len(states) || active > 0 {
		if ctx.Err() != nil {
			next = len(states)
		}
		limit := workers
		if idle != nil && !idle() {
			limit = 1
		}
		if next < len(states) && active < limit {
			state := states[next]
			next++
			active++
			go func() { results <- outcome{state, run(state)} }()
			continue
		}
		if active > 0 {
			result := <-results
			active--
			report(result.state, result.err)
		}
	}
}

// A batch owns immutable object storage, not every session writer. Ordinary
// append I/O stays available; each retirement takes only its own writer lease.
type nativeRetirementBatch struct {
	mu            sync.Mutex
	store         string
	lock          *storage.OperationLock
	resolver      *pack.Resolver
	closed        bool
	verifications int
	initialError  error
}

func (b *nativeRetirementBatch) open(ctx context.Context, store string, state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, nil, errors.New("native retirement batch is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if b.initialError != nil {
		return nil, nil, b.initialError
	}
	store = filepath.Clean(store)
	if b.resolver == nil {
		lock, err := storage.AcquireOperationLock(store, "objects")
		if err != nil {
			if errors.Is(err, storage.ErrOperationLockHeld) {
				err = fmt.Errorf("%w: %v", storage.ErrManagedSessionBusy, err)
			}
			b.initialError = err
			return nil, nil, err
		}
		resolver, err := pack.Open(store, pack.OpenOptions{})
		if err != nil {
			lock.Close()
			b.initialError = err
			return nil, nil, err
		}
		report, err := pack.Doctor(ctx, store)
		if err == nil && (report.ManifestCount == 0 || report.IssueCount != 0 || report.VerifiedManifestCount != report.ManifestCount) {
			err = errors.New("batch pack-only recovery proof is incomplete")
		}
		if err == nil {
			folded, e := fold.DoctorWithOptions(ctx, store, fold.DoctorOptions{Reader: resolver})
			err = e
			if err == nil && (folded.IssueCount != 0 || folded.ManifestCount != report.ManifestCount || folded.VerifiedManifestCount != folded.ManifestCount) {
				err = errors.New("batch fold pack-only proof is incomplete")
			}
		}
		if err != nil {
			resolver.Close()
			lock.Close()
			b.initialError = err
			return nil, nil, err
		}
		b.store, b.lock, b.resolver = store, lock, resolver
		b.verifications++
	}
	if b.store != store {
		return nil, nil, errors.New("native retirement batch cannot cross stores")
	}
	generation, err := pack.CurrentGeneration(store)
	if err != nil {
		return nil, nil, err
	}
	if generation != b.resolver.Generation() {
		return nil, nil, errors.New("pack changed during native retirement batch")
	}
	if err := storage.ValidateManagedSessionReference(storage.ManagedSessionReference{StoreDir: store, SessionID: state.SessionID, ManifestPath: state.ManifestPath, ManifestSHA256: state.ManifestSHA256, BaseBytes: state.BaseBytes, BaseSHA256: state.BaseSHA256}); err != nil {
		return nil, nil, fmt.Errorf("refresh native retirement manifest: %w", err)
	}
	// Each caller gets its own resolver reference; closing it cannot release the
	// batch's generation lease. Materialization still verifies this session.
	return openManagedSession(ctx, store, state)
}

func (b *nativeRetirementBatch) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var err error
	if b.resolver != nil {
		err = b.resolver.Close()
	}
	if b.lock != nil {
		err = errors.Join(err, b.lock.Close())
	}
	return err
}

func runInProcessNativeRetirement(ctx context.Context, args []string) error {
	command := newFSRetireNativeCommand()
	command.SetContext(ctx)
	command.SetArgs(args[2:])
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	return command.Execute()
}
