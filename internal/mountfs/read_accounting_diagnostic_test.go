package mountfs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fskitproto"
)

// A read that delivers correct bytes must always be accounted. The menu-bar app
// reports measured transfer rates from this counter, so a silently unaccounted
// read under-reports real work. This reproduces under CPU contention, so the
// test captures the server's transport records and reports which path was taken
// when the count does not match.
func TestNativeFSKitReadAccountingMatchesDeliveredBytes(t *testing.T) {
	for attempt := 0; attempt < 10; attempt++ {
		records, totals, delivered := runAccountedNativeFSKitRead(t)
		if delivered != 26 {
			t.Fatalf("attempt %d delivered %d bytes, want 26", attempt, delivered)
		}
		if totals.ReadBytes != 26 {
			t.Fatalf("attempt %d: delivered=%d but ReadBytes=%d WrittenBytes=%d\ntransport records:\n%s",
				attempt, delivered, totals.ReadBytes, totals.WrittenBytes, strings.Join(records, "\n"))
		}
	}
}

func runAccountedNativeFSKitRead(t *testing.T) ([]string, IOActivityTotals, int) {
	t.Helper()
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	for _, directory := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(nativeRoot, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	activity := &IOActivityCounter{}

	var mu sync.Mutex
	var records []string
	socketRoot := shortNativeFSKitTestDir(t, "cfa-")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	options := NativeFSKitServerOptions{
		SocketPath: filepath.Join(socketRoot, "daemon.sock"), ResourcePath: filepath.Join(root, "resource.bin"),
		Token: bytes.Repeat([]byte{0x42}, 32), Generation: 77, BuildSHA256: strings.Repeat("a", 64),
		Activity: activity,
		Recorder: func(line string) {
			mu.Lock()
			records = append(records, line)
			mu.Unlock()
		},
	}
	go func() { done <- ServeNativeFSKit(ctx, filesystem, options) }()
	t.Cleanup(func() { cancel(); <-done })

	client := dialAccountedNativeFSKitClient(t, options.ResourcePath, done, cancel)
	defer client.Close()

	for _, directory := range []string{"/sessions/2026", "/sessions/2026/08", "/sessions/2026/08/19"} {
		encoder := fskitproto.NewEncoder(128)
		encoder.String(directory)
		encoder.Uint32(0o700)
		if _, err := client.Call(fskitproto.OpMkdir, encoder.Data()); err != nil {
			t.Fatal(err)
		}
	}
	create := fskitproto.NewEncoder(128)
	create.String("/sessions/2026/08/19/rollout.jsonl")
	create.Uint32(uint32(os.O_RDWR | os.O_APPEND))
	createdPayload, err := client.Call(fskitproto.OpCreate, create.Data())
	if err != nil {
		t.Fatal(err)
	}
	createdDecoder := fskitproto.NewDecoder(createdPayload)
	handle, err := createdDecoder.Uint64()
	if err != nil {
		t.Fatal(err)
	}

	first := []byte("{\"record\":1}\n")
	second := []byte("{\"record\":2}\n")
	writeNativeFSKitTestPayload(t, client, handle, 0, first)
	writeNativeFSKitTestPayload(t, client, handle, int64(len(first)), second)
	callNativeFSKitHandle(t, client, fskitproto.OpFsync, handle)

	read := fskitproto.NewEncoder(24)
	read.Uint64(handle)
	read.Int64(0)
	read.Uint32(4096)
	readPayload, err := client.Call(fskitproto.OpRead, read.Data())
	if err != nil {
		t.Fatal(err)
	}
	got, err := fskitproto.NewDecoder(readPayload).Bytes(4096)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), first...), second...)
	if !bytes.Equal(got, want) {
		t.Fatalf("visible bytes = %q, want %q", got, want)
	}
	// Snapshot with no barrier first: the server records the read after it has
	// written the response frame, so an immediate snapshot can legitimately race
	// the accounting.
	immediate := activity.Snapshot()
	callNativeFSKitHandle(t, client, fskitproto.OpRelease, handle)
	time.Sleep(20 * time.Millisecond)
	settled := activity.Snapshot()
	if immediate.ReadBytes != settled.ReadBytes {
		t.Logf("accounting lagged the response: immediate=%d settled=%d", immediate.ReadBytes, settled.ReadBytes)
	}

	mu.Lock()
	captured := make([]string, 0, len(records))
	for _, line := range records {
		if strings.Contains(line, "read") || strings.Contains(line, "transport") {
			captured = append(captured, line)
		}
	}
	mu.Unlock()
	return captured, settled, len(got)
}

func dialAccountedNativeFSKitClient(t *testing.T, resourcePath string, done chan error, cancel func()) *fskitproto.Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case serveErr := <-done:
			cancel()
			t.Fatalf("FSKit test server exited during startup: %v", serveErr)
		default:
		}
		client, dialErr := fskitproto.DialResource(resourcePath, 100*time.Millisecond)
		if dialErr == nil {
			return client
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatal("FSKit test server did not accept a connection")
	return nil
}
