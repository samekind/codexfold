package mountfs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samekind/codexfold/internal/fskitproto"
)

// Keep one bounded, local stack snapshot when a real file operation stalls.
// A healthy heartbeat is not evidence that request-handling goroutines move.
// No request payload, session content or credentials are included.
type nativeFSKitStallDiagnostics struct {
	path  string
	after time.Duration
	mu    sync.Mutex
	last  time.Time
}

func (d *nativeFSKitStallDiagnostics) watch(op fskitproto.Op, request uint64) func() {
	if d == nil || d.path == "" {
		return func() {}
	}
	after := d.after
	if after <= 0 {
		after = 10 * time.Second
	}
	started := time.Now()
	var active atomic.Bool
	active.Store(true)
	timer := time.AfterFunc(after, func() {
		if !active.Load() {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if time.Since(d.last) < time.Minute || !active.Load() {
			return
		}
		buffer := make([]byte, 4<<20)
		n := runtime.Stack(buffer, true)
		header := fmt.Sprintf("CodexFold blocked I/O captured=%s pid=%d operation=%s request=%d elapsed=%s truncated=%t\n", time.Now().UTC().Format(time.RFC3339Nano), os.Getpid(), nativeFSKitOperationName(op), request, time.Since(started), n == len(buffer))
		if err := os.MkdirAll(filepath.Dir(d.path), 0700); err != nil {
			return
		}
		f, err := os.OpenFile(d.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		_, err = f.WriteString(header)
		if err == nil {
			_, err = f.Write(buffer[:n])
		}
		closeErr := f.Close()
		if err == nil && closeErr == nil {
			d.last = time.Now()
		}
	})
	return func() { active.Store(false); timer.Stop() }
}
