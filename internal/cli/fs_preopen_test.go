package cli

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestPrepareManagedOpensRunsIndependentSessionsConcurrently(t *testing.T) {
	states := make([]vfs.SessionState, 24)
	for index := range states {
		states[index].SessionID = fmt.Sprintf("session-%02d", index)
	}
	var active, peak, opened atomic.Int32
	open := func(state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
		current := active.Add(1)
		for prior := peak.Load(); current > prior && !peak.CompareAndSwap(prior, current); prior = peak.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		opened.Add(1)
		return nil, nil, fmt.Errorf("expected %s", state.SessionID)
	}
	prepared := prepareManagedOpens(context.Background(), states, func(state vfs.SessionState) bool {
		return state.SessionID != "session-00"
	}, open, 4)
	defer closePreparedManagedOpens(prepared)
	if peak.Load() < 2 || peak.Load() > 4 || opened.Load() != 23 {
		t.Fatalf("peak=%d opened=%d", peak.Load(), opened.Load())
	}
	if _, _, err := prepared[0].take(states[0], open); err == nil {
		t.Fatal("unprepared session did not use the fallback opener")
	}
	if _, _, err := prepared[1].take(states[1], open); err == nil || !strings.Contains(err.Error(), "session-01") {
		t.Fatalf("prepared error was not returned: %v", err)
	}
	if opened.Load() != 24 {
		t.Fatalf("prepared open was repeated: opened=%d", opened.Load())
	}
}
