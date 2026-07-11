package testfs

import (
	"errors"
	"sync"
)

type Faults struct {
	mu    sync.Mutex
	phase string
	fired bool
}

func NewFaults(phase string) *Faults { return &Faults{phase: phase} }

func (f *Faults) Hook(phase string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fired && phase == f.phase {
		f.fired = true
		return errors.New("injected fault at " + phase)
	}
	return nil
}

func (f *Faults) Fired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}
