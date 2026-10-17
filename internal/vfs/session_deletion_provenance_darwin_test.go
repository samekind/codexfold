//go:build darwin

package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDeletionPurgeProvenanceComparisonIsNarrow(t *testing.T) {
	old := SessionDeletionPurgeXattrs{Count: 1, Bytes: 11, SHA256: strings.Repeat("a", 64)}
	current := old
	current.SHA256 = strings.Repeat("b", 64)
	if !sameDeletionPurgeXattrs(old, current) || !sameDeletionPurgeXattrs(old, SessionDeletionPurgeXattrs{SHA256: digestStateBytes(nil)}) {
		t.Fatal("OS provenance transition rejected")
	}
	for _, changed := range []SessionDeletionPurgeXattrs{
		{Count: 2, Bytes: 11, SHA256: current.SHA256},
		{Count: 1, Bytes: 12, SHA256: current.SHA256},
		{Count: 0, Bytes: 0, SHA256: current.SHA256},
		{Count: 1, Bytes: 11, SHA256: "invalid"},
	} {
		if sameDeletionPurgeXattrs(old, changed) {
			t.Fatalf("unknown metadata accepted: %+v", changed)
		}
	}
}

func TestSessionDeletionPurgeResumesLegacyProvenanceReceipts(t *testing.T) {
	for _, phase := range []string{sessionDeletionPurgePrepared, sessionDeletionPurgeRenamed} {
		t.Run(phase, func(t *testing.T) {
			root, _, tombstone := sessionDeletionFixture(t)
			changed := false
			sessionDeletionPurgeHook = func(got string) error {
				if got != phase || changed {
					return nil
				}
				changed = true
				r, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
				if err != nil {
					return err
				}
				old := SessionDeletionPurgeXattrs{Count: 1, Bytes: 11, SHA256: strings.Repeat("a", 64)}
				r.QuarantineRootXattrs = old
				for i := range r.Entries {
					r.Entries[i].Xattrs = old
				}
				h := sha256.New()
				fmt.Fprintf(h, "R\x00%o\x00%s\x00%s\n", r.QuarantineRootMode, deletionPurgeObjectHashFields(r.QuarantineRootObject), deletionPurgeXattrHashFields(old))
				for _, e := range r.Entries {
					if e.Kind == "directory" {
						fmt.Fprintf(h, "D\x00%s\x00%o\x00%s\x00%s\n", e.Path, e.Mode, deletionPurgeObjectHashFields(e.Object), deletionPurgeXattrHashFields(e.Xattrs))
					} else {
						fmt.Fprintf(h, "F\x00%s\x00%o\x00%s\x00%s\x00%d\x00%s\n", e.Path, e.Mode, deletionPurgeObjectHashFields(e.Object), deletionPurgeXattrHashFields(e.Xattrs), e.Bytes, e.SHA256)
					}
				}
				r.QuarantineTreeSHA256 = hex.EncodeToString(h.Sum(nil))
				if err := replaceSessionDeletionPurge(root, r); err != nil {
					return err
				}
				return fmt.Errorf("stop after provenance fixture")
			}
			t.Cleanup(func() { sessionDeletionPurgeHook = nil })
			if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
				t.Fatalf("fixture stop completed=%t err=%v", completed, err)
			}
			sessionDeletionPurgeHook = nil
			if !changed {
				t.Fatal("receipt fixture was not changed")
			}
			if completed, err := AdvanceSessionDeletion(root, tombstone); !completed || err != nil {
				t.Fatalf("resume provenance completed=%t err=%v", completed, err)
			}
			assertSessionDeletionPurged(t, root, tombstone)
		})
	}
}

func TestSessionDeletionPurgeStillPreservesForeignXattr(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)
	file := filepath.Join(tombstone.RetiredSessionPath, "delta.jsonl")
	if err := unix.Setxattr(file, "com.codexfold.foreign", []byte("preserve"), 0); err != nil {
		t.Fatal(err)
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("foreign xattr purged completed=%t err=%v", completed, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("foreign metadata object removed: %v", err)
	}
}
