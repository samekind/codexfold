package mountfs

import (
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fskitproto"
	"github.com/samekind/codexfold/internal/mountid"
	"github.com/samekind/codexfold/internal/storage"
)

func TestNativeRequestHealthOnlyAlertsForUnfinishedAccess(t *testing.T) {
	h := &nativeFSKitRequestHealth{}
	stopPing := h.begin(fskitproto.OpPing)
	stopPing()
	if _, ok := h.stalled(time.Now().Add(time.Minute)); ok {
		t.Fatal("idle health check raised an access incident")
	}
	stop := h.begin(fskitproto.OpWrite)
	if _, ok := h.stalled(time.Now()); ok {
		t.Fatal("fast write raised an incident")
	}
	for id, request := range h.pending {
		request.started = time.Now().Add(-11 * time.Second)
		h.pending[id] = request
	}
	options := NativeFSKitServerOptions{requestHealth: h}
	tracker := newNativeFSKitDaemonStatusTracker(options)
	bad := tracker.snapshot(options, mountid.Identity{}, "healthy", "available", "")
	if bad.State != "recovering" || bad.IncidentID == "" || bad.Detail == "" || bad.ElapsedMilliseconds < 10000 {
		t.Fatalf("blocked write not exposed: %+v", bad)
	}
	stop()
	ok := tracker.snapshot(options, mountid.Identity{}, "healthy", "available", "")
	if ok.State != "healthy" || ok.IncidentID != bad.IncidentID {
		t.Fatalf("completed request did not supply matching recovery: %+v", ok)
	}
}

func TestStorageBudgetFailureUsesNoSpaceErrno(t *testing.T) {
	if got := errnoFor(storage.ErrBudgetExceeded); got != syscall.ENOSPC {
		t.Fatalf("budget rejection becomes %v, want ENOSPC", got)
	}
}
