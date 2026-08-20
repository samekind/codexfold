package mountfs

import (
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/vfs"
)

func transientLoadFilesystem(sleep func(time.Duration)) *Filesystem {
	filesystem := New()
	filesystem.loadRetryBudget = 50 * time.Millisecond
	filesystem.loadRetryInterval = time.Millisecond
	filesystem.loadRetrySleep = sleep
	return filesystem
}

// A managed route stays registered while an enrollment cycle swaps pack
// generations underneath it. The product contract allows the access to pause,
// but never to surface a hard I/O failure to Codex inside that window.
func TestFilesystemManagedAccessSurvivesTransientLoaderFailure(t *testing.T) {
	source := []byte("managed-history\n")
	session := mountSessionFixture(t, "swapping", source)
	transient := errors.New("read pack CURRENT: pack generation swap in progress")
	for _, testCase := range []struct {
		name   string
		access func(*Filesystem) syscall.Errno
	}{
		{name: "getattr", access: func(filesystem *Filesystem) syscall.Errno {
			_, errno := filesystem.Getattr("/swapping.jsonl")
			return errno
		}},
		{name: "open", access: func(filesystem *Filesystem) syscall.Errno {
			handle, errno := filesystem.Open("/swapping.jsonl", os.O_RDONLY)
			if errno == 0 {
				_ = filesystem.Release(handle)
			}
			return errno
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			filesystem := transientLoadFilesystem(func(time.Duration) {})
			calls := 0
			filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
				calls++
				if calls == 1 {
					return nil, transient
				}
				return session, nil
			})
			if errno := testCase.access(filesystem); errno != 0 {
				t.Fatalf("access during transient store swap errno=%v (%d) after %d loader calls, want the access to wait for recovery and succeed", errno, int(errno), calls)
			}
			if calls < 2 {
				t.Fatalf("loader calls = %d, want the failed attempt to be retried", calls)
			}
		})
	}
}

// A classified failure keeps its exact errno and must not consume the retry
// budget. A permanently absent or forbidden route still fails immediately.
func TestFilesystemManagedAccessDoesNotRetryClassifiedLoaderFailures(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want syscall.Errno
	}{
		{name: "missing", err: os.ErrNotExist, want: syscall.ENOENT},
		{name: "forbidden", err: os.ErrPermission, want: syscall.EACCES},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			filesystem := transientLoadFilesystem(func(time.Duration) {
				t.Fatal("classified loader failure must not wait")
			})
			calls := 0
			filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
				calls++
				return nil, testCase.err
			})
			if _, errno := filesystem.Getattr("/classified.jsonl"); errno != testCase.want {
				t.Fatalf("errno = %v, want %v", errno, testCase.want)
			}
			if calls != 1 {
				t.Fatalf("loader calls = %d, want 1", calls)
			}
		})
	}
}

// A failure that outlives the budget still reaches Codex, which is what arms
// the ten-second incident report instead of hiding a real outage forever.
func TestFilesystemManagedAccessReportsFailureAfterBudget(t *testing.T) {
	filesystem := transientLoadFilesystem(nil)
	filesystem.loadRetryBudget = 20 * time.Millisecond
	filesystem.loadRetryInterval = 2 * time.Millisecond
	filesystem.loadRetrySleep = time.Sleep
	calls := 0
	filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
		calls++
		return nil, errors.New("pack index unreadable")
	})
	started := time.Now()
	_, errno := filesystem.Getattr("/never.jsonl")
	elapsed := time.Since(started)
	if errno != syscall.EIO {
		t.Fatalf("errno = %v, want EIO after the budget", errno)
	}
	if calls < 2 {
		t.Fatalf("loader calls = %d, want repeated attempts inside the budget", calls)
	}
	if elapsed < filesystem.loadRetryBudget {
		t.Fatalf("gave up after %v, want at least the %v budget", elapsed, filesystem.loadRetryBudget)
	}
}

// The service loader runs a full store discovery per attempt while holding its
// own lock, so the retry must be bounded by an attempt cap and not only by the
// time budget. An unbounded short-interval retry turns one stalled session into
// a discovery storm.
func TestFilesystemSessionLoadRetryIsBoundedByAttemptCap(t *testing.T) {
	filesystem := New()
	filesystem.loadRetryInterval = time.Nanosecond
	filesystem.loadRetrySleep = func(time.Duration) {}
	calls := 0
	filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
		calls++
		return nil, errors.New("pack generation changed while it was opened")
	})
	if _, errno := filesystem.Getattr("/storm.jsonl"); errno != syscall.EIO {
		t.Fatalf("errno = %v, want EIO", errno)
	}
	if calls > maximumSessionLoadAttempts {
		t.Fatalf("loader calls = %d, want at most the %d attempt cap", calls, maximumSessionLoadAttempts)
	}
	if calls < 2 {
		t.Fatalf("loader calls = %d, want the transient to be retried", calls)
	}
}

// The recorder must name the session, the attempt count, and the elapsed time,
// and must emit one line per transition rather than one per attempt. A
// per-attempt line is what turned the previous outage into thousands of
// identical lines with no session ID in any of them.
func TestFilesystemSessionLoadRecorderReportsTransitionsOnly(t *testing.T) {
	t.Run("deferred then recovered", func(t *testing.T) {
		session := mountSessionFixture(t, "recovering", []byte("recovering\n"))
		filesystem := transientLoadFilesystem(func(time.Duration) {})
		var lines []string
		filesystem.SetSessionLoadRecorder(func(line string) { lines = append(lines, line) })
		calls := 0
		filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
			calls++
			if calls <= 4 {
				return nil, errors.New("pack generation swap in progress")
			}
			return session, nil
		})
		if _, errno := filesystem.Getattr("/recovering.jsonl"); errno != 0 {
			t.Fatalf("errno = %v, want the access to recover", errno)
		}
		if len(lines) != 2 {
			t.Fatalf("recorded lines = %d (%v), want exactly one deferred and one recovered line", len(lines), lines)
		}
		if !strings.Contains(lines[0], "event=session_load_deferred") || !strings.Contains(lines[0], "session=recovering") {
			t.Fatalf("deferred line = %q", lines[0])
		}
		if !strings.Contains(lines[1], "event=session_load_recovered") || !strings.Contains(lines[1], "attempts=5") {
			t.Fatalf("recovered line = %q", lines[1])
		}
		for _, line := range lines {
			if !strings.Contains(line, "elapsed_ms=") || !strings.Contains(line, "errno=") {
				t.Fatalf("line %q lacks the fields needed to correlate an incident", line)
			}
		}
	})

	t.Run("failed after budget", func(t *testing.T) {
		filesystem := transientLoadFilesystem(func(time.Duration) {})
		filesystem.loadRetryBudget = 5 * time.Millisecond
		var lines []string
		filesystem.SetSessionLoadRecorder(func(line string) { lines = append(lines, line) })
		filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
			return nil, errors.New("pack index unreadable")
		})
		if _, errno := filesystem.Getattr("/doomed.jsonl"); errno != syscall.EIO {
			t.Fatalf("errno = %v, want EIO", errno)
		}
		if len(lines) != 2 {
			t.Fatalf("recorded lines = %d (%v), want one deferred and one failed line", len(lines), lines)
		}
		if !strings.Contains(lines[1], "event=session_load_failed") || !strings.Contains(lines[1], "session=doomed") {
			t.Fatalf("failed line = %q", lines[1])
		}
	})

	t.Run("classified failure is not an incident", func(t *testing.T) {
		filesystem := transientLoadFilesystem(func(time.Duration) {})
		recorded := 0
		filesystem.SetSessionLoadRecorder(func(string) { recorded++ })
		filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
			return nil, os.ErrNotExist
		})
		if _, errno := filesystem.Getattr("/absent.jsonl"); errno != syscall.ENOENT {
			t.Fatalf("errno = %v, want ENOENT", errno)
		}
		if recorded != 0 {
			t.Fatalf("recorded lines = %d, want 0 for a classified failure", recorded)
		}
	})
}

// The retry must release the global load mutex between attempts. One session
// stalled inside a store transition may not block first access to a healthy
// session, because one bad session must never stall the whole service.
func TestFilesystemStalledSessionLoadDoesNotBlockHealthySessions(t *testing.T) {
	healthy := mountSessionFixture(t, "healthy", []byte("healthy\n"))
	filesystem := transientLoadFilesystem(time.Sleep)
	filesystem.loadRetryBudget = 2 * time.Second
	filesystem.loadRetryInterval = time.Millisecond
	release := make(chan struct{})
	var once sync.Once
	filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
		if sessionID == "stalled" {
			once.Do(func() { close(release) })
			return nil, errors.New("pack generation swap in progress")
		}
		return healthy, nil
	})

	stalled := make(chan syscall.Errno, 1)
	go func() {
		_, errno := filesystem.Getattr("/stalled.jsonl")
		stalled <- errno
	}()
	<-release

	done := make(chan syscall.Errno, 1)
	go func() {
		_, errno := filesystem.Getattr("/healthy.jsonl")
		done <- errno
	}()
	select {
	case errno := <-done:
		if errno != 0 {
			t.Fatalf("healthy session errno=%v while another session was stalled", errno)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy session load blocked behind a stalled session load")
	}
	if errno := <-stalled; errno != syscall.EIO {
		t.Fatalf("stalled session errno = %v, want EIO after its budget", errno)
	}
}
