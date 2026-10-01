package cli

import (
	"context"
	"sync"

	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/vfs"
)

type preparedManagedOpen struct {
	state    vfs.SessionState
	managed  *vfs.Session
	resolver *pack.Resolver
	err      error
}

// prepareManagedOpens overlaps independent per-session crash recovery during a
// cold start. Publication still happens in the original single-threaded order,
// and each prepared owner is either transferred exactly once or closed.
func prepareManagedOpens(
	ctx context.Context,
	states []vfs.SessionState,
	selected func(vfs.SessionState) bool,
	open func(vfs.SessionState) (*vfs.Session, *pack.Resolver, error),
	workers int,
) []preparedManagedOpen {
	prepared := make([]preparedManagedOpen, len(states))
	if workers < 1 {
		workers = 1
	}
	queue := make(chan int)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range queue {
				state := states[index]
				managed, resolver, err := open(state)
				prepared[index] = preparedManagedOpen{state: state, managed: managed, resolver: resolver, err: err}
			}
		}()
	}
	for index, state := range states {
		if ctx.Err() != nil {
			break
		}
		if selected(state) {
			queue <- index
		}
	}
	close(queue)
	group.Wait()
	return prepared
}

func (p *preparedManagedOpen) take(state vfs.SessionState, fallback func(vfs.SessionState) (*vfs.Session, *pack.Resolver, error)) (*vfs.Session, *pack.Resolver, error) {
	if p.managed == nil && p.err == nil {
		return fallback(state)
	}
	if p.state != state {
		_ = p.close()
		return fallback(state)
	}
	managed, resolver, err := p.managed, p.resolver, p.err
	*p = preparedManagedOpen{}
	return managed, resolver, err
}

func (p *preparedManagedOpen) close() error {
	resolver := p.resolver
	*p = preparedManagedOpen{}
	if resolver != nil {
		return resolver.Close()
	}
	return nil
}

func closePreparedManagedOpens(prepared []preparedManagedOpen) {
	for index := range prepared {
		_ = prepared[index].close()
	}
}
