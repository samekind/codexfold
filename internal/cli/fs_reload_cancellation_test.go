package cli

import (
	"context"
	"testing"
	"time"
)

func TestManagedReloadCancellationDoesNotPublishOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loads, reports := 0, 0
	runManagedReloadLoop(ctx, time.Millisecond, time.Millisecond, func() error {
		loads++
		cancel()
		return context.Canceled
	}, func(error) { reports++ })
	if loads != 1 || reports != 0 { t.Fatalf("normal shutdown reported an outage: loads=%d reports=%d", loads, reports) }
}
