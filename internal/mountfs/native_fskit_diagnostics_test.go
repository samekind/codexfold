package mountfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fskitproto"
)

func TestNativeFSKitStallDiagnosticCapturesOnlyBlockedOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	d := &nativeFSKitStallDiagnostics{path: path, after: 15 * time.Millisecond}
	stop := d.watch(fskitproto.OpPing, 1)
	stop()
	time.Sleep(25 * time.Millisecond)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fast request produced a dump: %v", err)
	}
	stop = d.watch(fskitproto.OpRead, 2)
	defer stop()
	deadline := time.Now().Add(time.Second)
	for {
		d.mu.Lock()
		data, err := os.ReadFile(path)
		d.mu.Unlock()
		if err == nil && strings.Contains(string(data), "goroutine ") && strings.Contains(string(data), "request=2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing stalled I/O dump: %v %q", err, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
