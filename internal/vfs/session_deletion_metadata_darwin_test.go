//go:build darwin

package vfs

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSessionDeletionPurgePreservesUnprovedDarwinXattrs(t *testing.T) {
	for _, attribute := range []string{"com.example.codexfold-foreign", "com.apple.ResourceFork"} {
		t.Run(strings.ReplaceAll(attribute, ".", "-"), func(t *testing.T) {
			root, _, tombstone := sessionDeletionFixture(t)
			stageSessionDeletionQuarantine(t, tombstone)
			target := tombstone.RetiredManifestPath
			value := []byte("unproved extended metadata must survive")
			if err := unix.Setxattr(target, attribute, value, 0); err != nil {
				t.Skipf("cannot set %s: %v", attribute, err)
			}

			if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
				t.Fatalf("xattr purge completed=%t err=%v", completed, err)
			}
			got := make([]byte, len(value))
			read, err := unix.Getxattr(target, attribute, got)
			if err != nil || read != len(value) || !bytes.Equal(got[:read], value) {
				t.Fatalf("xattr changed: got=%q read=%d err=%v", got, read, err)
			}
			if _, err := os.Lstat(SessionDeletionPurgePath(root, tombstone.SessionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("purge receipt authorized unproved xattr content: %v", err)
			}
		})
	}
}

func TestSessionDeletionPurgePreservesDarwinACL(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)
	target := tombstone.RetiredManifestPath
	current, err := user.Current()
	if err != nil {
		t.Skipf("cannot determine current user: %v", err)
	}
	entry := current.Username + " allow read"
	if output, err := exec.Command("/bin/chmod", "+a", entry, target).CombinedOutput(); err != nil {
		t.Skipf("cannot set Darwin ACL: %v: %s", err, output)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("ACL purge completed=%t err=%v", completed, err)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("ACL-bearing file was not preserved: %v", err)
	}
	output, err := exec.Command("/bin/ls", "-le", target).CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("allow read")) {
		t.Fatalf("ACL changed: err=%v output=%s", err, output)
	}
}

func TestSessionDeletionPurgePreservesDarwinBSDFlags(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)
	target := tombstone.RetiredManifestPath
	if err := unix.Chflags(target, unix.UF_NODUMP); err != nil {
		t.Skipf("cannot set Darwin BSD flags: %v", err)
	}
	t.Cleanup(func() { _ = unix.Chflags(target, 0) })

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("BSD-flag purge completed=%t err=%v", completed, err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("BSD-flag file was not preserved: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Flags&unix.UF_NODUMP == 0 {
		t.Fatalf("BSD flags changed: info=%#v", info.Sys())
	}
}

func TestSessionDeletionPurgeRejectsXattrAddedAfterReceiptPublication(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stop := errors.New("stop after exact purge receipt")
	sessionDeletionPurgeHook = func(phase string) error {
		if phase == sessionDeletionPurgePrepared {
			return stop
		}
		return nil
	}
	t.Cleanup(func() { sessionDeletionPurgeHook = nil })
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
		t.Fatalf("prepared purge completed=%t err=%v", completed, err)
	}
	receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(tombstone.RetiredSessionPath), "manifest.json")
	value := []byte("post-receipt foreign metadata")
	if err := unix.Setxattr(target, "com.example.codexfold-after-proof", value, 0); err != nil {
		t.Skipf("cannot set post-receipt xattr: %v", err)
	}
	sessionDeletionPurgeHook = nil

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("post-receipt xattr purge completed=%t err=%v receipt=%#v", completed, err, receipt)
	}
	got := make([]byte, len(value))
	read, err := unix.Getxattr(target, "com.example.codexfold-after-proof", got)
	if err != nil || read != len(value) || !bytes.Equal(got[:read], value) {
		t.Fatalf("post-receipt xattr changed: got=%q read=%d err=%v", got, read, err)
	}
}
