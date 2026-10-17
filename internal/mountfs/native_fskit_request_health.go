package mountfs

import (
	"sync"
	"time"

	"github.com/samekind/codexfold/internal/fskitproto"
)

type nativeFSKitPendingRequest struct {
	started time.Time
	op      fskitproto.Op
}

// Only unfinished user filesystem requests establish an access stall. A slow
// background inventory/reload or an idle authenticated socket does not.
type nativeFSKitRequestHealth struct {
	mu      sync.Mutex
	next    uint64
	pending map[uint64]nativeFSKitPendingRequest
}

func (h *nativeFSKitRequestHealth) begin(op fskitproto.Op) func() {
	if h == nil || op == fskitproto.OpHello || op == fskitproto.OpPing {
		return func() {}
	}
	h.mu.Lock()
	if h.pending == nil {
		h.pending = make(map[uint64]nativeFSKitPendingRequest)
	}
	h.next++
	id := h.next
	h.pending[id] = nativeFSKitPendingRequest{started: time.Now(), op: op}
	h.mu.Unlock()
	return func() { h.mu.Lock(); delete(h.pending, id); h.mu.Unlock() }
}

func (h *nativeFSKitRequestHealth) stalled(now time.Time) (nativeFSKitPendingRequest, bool) {
	if h == nil {
		return nativeFSKitPendingRequest{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var oldest nativeFSKitPendingRequest
	for _, request := range h.pending {
		if now.Sub(request.started) >= 10*time.Second && (oldest.started.IsZero() || request.started.Before(oldest.started)) {
			oldest = request
		}
	}
	return oldest, !oldest.started.IsZero()
}
