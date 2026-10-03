package cli

import (
	"context"
	"testing"
	"time"
)

func TestManagedRecoveryRequestWakesHealthyReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)
	loaded := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runManagedReloadLoopAfterInitial(ctx, time.Minute, 10*time.Minute, nil, wake,
			func() error { loaded <- struct{}{}; return nil }, func(error) {})
	}()
	wake <- struct{}{}
	select {
	case <-loaded:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery request did not wake the healthy backend's long poll")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not stop after cancellation")
	}
}
