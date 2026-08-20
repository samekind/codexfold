package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/storage"
)

// A lease leaked by a killed reader carries the process-ID payload that
// storage.AcquireLease writes. Rejecting that payload as unknown content pinned
// the generation forever, because the writer never produces an empty lease.
func TestValidatePackRemovalLeaseTreeAcceptsALeakedLease(t *testing.T) {
	root := t.TempDir()
	leaked := filepath.Join(root, storage.LeaseFilePrefix+"resolver-1029-70e8c8871da8c17b")
	if err := os.WriteFile(leaked, []byte("1029\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePackRemovalLeaseTree(root); err != nil {
		t.Fatalf("leaked lease blocked generation removal: %v", err)
	}
}

func TestValidatePackRemovalLeaseTreeRejectsUnknownContent(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		file    string
		content string
	}{
		{name: "foreign name", file: "resolver-state.json", content: "1029\n"},
		{name: "non numeric payload", file: storage.LeaseFilePrefix + "resolver-1-a", content: "held by codex\n"},
		{name: "oversized payload", file: storage.LeaseFilePrefix + "resolver-2-b", content: strings.Repeat("9", 128)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, testCase.file), []byte(testCase.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := validatePackRemovalLeaseTree(root); err == nil {
				t.Fatal("unknown lease content must fail closed")
			}
		})
	}
}

// Liveness must come from the lock, never from the recorded process ID, because
// the system reuses process IDs. A held lease still blocks removal.
func TestPackGenerationRemovalRefusesWhileALeaseIsHeld(t *testing.T) {
	root := t.TempDir()
	lease, err := storage.AcquireLease(filepath.Join(root, "leases"), "resolver")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	active, err := storage.DirectoryHasActiveLease(filepath.Join(root, "leases"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("a held lease must be reported active")
	}
}

// A lease whose recorded process ID now belongs to an unrelated live process
// must not be treated as active. This is the exact state observed on the
// validation host, where a leaked lease recorded PID 1029 and that PID had been
// reused by a system daemon.
func TestLeakedLeaseWithAReusedProcessIDIsNotActive(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "leases")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	// PID 1 always exists, so a process-ID liveness check would call this active.
	if err := os.WriteFile(filepath.Join(directory, storage.LeaseFilePrefix+"resolver-1-deadbeefdeadbeef"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	active, err := storage.DirectoryHasActiveLease(directory, false)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("an unlocked lease must never be active, whatever process ID it records")
	}
	if err := validatePackRemovalLeaseTree(directory); err != nil {
		t.Fatalf("leaked lease with a reused process ID blocked removal: %v", err)
	}
}
