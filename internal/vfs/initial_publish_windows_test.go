//go:build windows

package vfs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInitialWriterLeaseSurvivesWindowsPublication(t *testing.T) {
	if path := os.Getenv("CODEXFOLD_TEST_PUBLISHED_LEASE"); path != "" {
		lease, err := acquireWriterLease(path)
		if !errors.Is(err, ErrWriterBusy) {
			if lease != nil {
				_ = unlockWriterFile(lease)
				_ = lease.Close()
			}
			t.Fatalf("published writer lease did not exclude child process: %v", err)
		}
		return
	}
	parent := t.TempDir()
	staging := filepath.Join(parent, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireInitialWriterLease(staging, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	final := filepath.Join(parent, "session-1")
	if err := os.Rename(staging, final); err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(final, "writer.lease")
	child := exec.Command(os.Args[0], "-test.run=^TestInitialWriterLeaseSurvivesWindowsPublication$")
	child.Env = append(os.Environ(), "CODEXFOLD_TEST_PUBLISHED_LEASE="+leasePath)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child writer exclusion: %v\n%s", err, output)
	}
	if err := unlockWriterFile(lease); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := acquireWriterLease(leasePath)
	if err != nil {
		t.Fatalf("writer after release: %v", err)
	}
	_ = unlockWriterFile(reopened)
	_ = reopened.Close()
	if _, err := os.Stat(filepath.Join(parent, initialSessionLockName("session-1")+".writer")); !os.IsNotExist(err) {
		t.Fatalf("temporary writer name remains: %v", err)
	}
}

func TestPublishedWriterLeaseSurvivesWindowsRetirement(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "session")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "writer.lease")
	lease, err := acquireWriterLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	retired := filepath.Join(parent, "retired-session")
	if err := os.Rename(directory, retired); err != nil {
		t.Fatalf("retire locked session: %v", err)
	}
	guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(retired, "writer.lease"))
	if err != nil || acquired {
		if guard != nil {
			_ = guard.Close()
		}
		t.Fatalf("retired lease lost exclusion: acquired=%v err=%v", acquired, err)
	}
	if err := unlockWriterFile(lease); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	guard, acquired, err = TryAcquireWriterLeaseGuardAtPath(filepath.Join(retired, "writer.lease"))
	if err != nil || !acquired {
		t.Fatalf("guard after retirement: acquired=%v err=%v", acquired, err)
	}
	defer guard.Close()
	if err := os.Rename(retired, directory); err != nil {
		t.Fatalf("restore guarded session: %v", err)
	}
}
