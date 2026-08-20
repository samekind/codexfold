package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidSessionDeletionPurgeObjectRequiresExplicitIdentityProof(t *testing.T) {
	darwin := SessionDeletionPurgeObject{
		IdentityProof:     sessionDeletionObjectProofDarwin,
		GenerationSource:  sessionDeletionGenerationSourceDarwin,
		BirthtimeSource:   sessionDeletionBirthtimeSourceDarwin,
		Device:            1,
		Inode:             2,
		BirthtimeUnixNano: 3,
		UID:               4,
		GID:               5,
	}
	linux := darwin
	linux.IdentityProof = sessionDeletionObjectProofLinux
	linux.GenerationSource = sessionDeletionGenerationSourceLinux
	linux.BirthtimeSource = sessionDeletionBirthtimeSourceLinux
	linux.Generation = 6

	tests := []struct {
		name   string
		object SessionDeletionPurgeObject
		valid  bool
	}{
		{name: "darwin", object: darwin, valid: true},
		{name: "linux", object: linux, valid: true},
		{name: "missing proof", object: SessionDeletionPurgeObject{Device: 1, Inode: 2, BirthtimeUnixNano: 3}},
		{name: "linux zero generation", object: func() SessionDeletionPurgeObject {
			object := linux
			object.Generation = 0
			return object
		}()},
		{name: "mixed proof and generation source", object: func() SessionDeletionPurgeObject {
			object := linux
			object.GenerationSource = sessionDeletionGenerationSourceDarwin
			return object
		}()},
		{name: "mixed proof and birthtime source", object: func() SessionDeletionPurgeObject {
			object := darwin
			object.BirthtimeSource = sessionDeletionBirthtimeSourceLinux
			return object
		}()},
		{name: "unknown proof", object: func() SessionDeletionPurgeObject {
			object := darwin
			object.IdentityProof = "unknown"
			return object
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validSessionDeletionPurgeObject(test.object); got != test.valid {
				t.Fatalf("validSessionDeletionPurgeObject()=%t, want %t for %#v", got, test.valid, test.object)
			}
		})
	}
}

func TestDeletionPurgeObjectHashBindsIdentityProofSources(t *testing.T) {
	object := SessionDeletionPurgeObject{
		IdentityProof:     sessionDeletionObjectProofDarwin,
		GenerationSource:  sessionDeletionGenerationSourceDarwin,
		BirthtimeSource:   sessionDeletionBirthtimeSourceDarwin,
		Device:            1,
		Inode:             2,
		BirthtimeUnixNano: 3,
		UID:               4,
		GID:               5,
	}
	baseline := deletionPurgeObjectHashFields(object)

	mutations := map[string]func(*SessionDeletionPurgeObject){
		"identity proof": func(candidate *SessionDeletionPurgeObject) { candidate.IdentityProof = sessionDeletionObjectProofLinux },
		"generation source": func(candidate *SessionDeletionPurgeObject) {
			candidate.GenerationSource = sessionDeletionGenerationSourceLinux
		},
		"birthtime source": func(candidate *SessionDeletionPurgeObject) {
			candidate.BirthtimeSource = sessionDeletionBirthtimeSourceLinux
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := object
			mutate(&candidate)
			if got := deletionPurgeObjectHashFields(candidate); got == baseline {
				t.Fatalf("identity proof mutation was not bound into the purge hash: %#v", candidate)
			}
		})
	}
}

func TestValidateSessionDeletionPurgeRejectsObjectsWithoutProof(t *testing.T) {
	root := t.TempDir()
	receipt := sessionDeletionPurgeIdentityReceipt(t, root)
	if err := validateSessionDeletionPurge(root, receipt); err != nil {
		t.Fatalf("valid identity receipt rejected: %v", err)
	}

	withoutRootProof := receipt
	withoutRootProof.QuarantineRootObject.IdentityProof = ""
	if err := validateSessionDeletionPurge(root, withoutRootProof); err == nil {
		t.Fatal("purge receipt without a root identity proof was accepted")
	}

	withoutEntryProof := receipt
	withoutEntryProof.Entries = append([]SessionDeletionPurgeEntry(nil), receipt.Entries...)
	withoutEntryProof.Entries[0].Object.IdentityProof = ""
	if err := validateSessionDeletionPurge(root, withoutEntryProof); err == nil {
		t.Fatal("purge receipt without an entry identity proof was accepted")
	}
}

func sessionDeletionPurgeIdentityReceipt(t *testing.T, root string) SessionDeletionPurge {
	t.Helper()
	object := SessionDeletionPurgeObject{
		IdentityProof:     sessionDeletionObjectProofDarwin,
		GenerationSource:  sessionDeletionGenerationSourceDarwin,
		BirthtimeSource:   sessionDeletionBirthtimeSourceDarwin,
		Device:            1,
		Inode:             2,
		BirthtimeUnixNano: 3,
		UID:               4,
		GID:               5,
	}
	xattrs := SessionDeletionPurgeXattrs{SHA256: digestStateBytes(nil)}
	fileBytes := []byte("file")
	fileDigest := sha256.Sum256(fileBytes)
	entries := []SessionDeletionPurgeEntry{
		{Path: "directory", Kind: "directory", Mode: 0o700, Object: object, Xattrs: xattrs},
		{Path: "file", Kind: "file", Mode: 0o600, Object: object, Xattrs: xattrs, Bytes: int64(len(fileBytes)), SHA256: hex.EncodeToString(fileDigest[:])},
	}
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, "R\x00"+strconv.FormatUint(0o700, 8)+"\x00"+deletionPurgeObjectHashFields(object)+"\x00"+deletionPurgeXattrHashFields(xattrs)+"\n")
	for _, entry := range entries {
		mode := strconv.FormatUint(uint64(entry.Mode), 8)
		switch entry.Kind {
		case "directory":
			_, _ = io.WriteString(hasher, "D\x00"+entry.Path+"\x00"+mode+"\x00"+deletionPurgeObjectHashFields(entry.Object)+"\x00"+deletionPurgeXattrHashFields(entry.Xattrs)+"\n")
		case "file":
			_, _ = io.WriteString(hasher, "F\x00"+entry.Path+"\x00"+mode+"\x00"+deletionPurgeObjectHashFields(entry.Object)+"\x00"+deletionPurgeXattrHashFields(entry.Xattrs)+"\x00"+strconv.FormatInt(entry.Bytes, 10)+"\x00"+entry.SHA256+"\n")
		}
	}
	operationToken := strings.Repeat("ab", 16)
	return SessionDeletionPurge{
		Version:              sessionDeletionPurgeVersion,
		Kind:                 sessionDeletionPurgeKind,
		SessionID:            "identity-proof",
		OperationToken:       operationToken,
		TombstoneSHA256:      digestStateBytes([]byte("tombstone")),
		QuarantineTreeSHA256: hex.EncodeToString(hasher.Sum(nil)),
		QuarantineBytes:      int64(len(fileBytes)),
		QuarantineEntries:    len(entries),
		QuarantineRootMode:   0o700,
		QuarantineRootObject: object,
		QuarantineRootXattrs: xattrs,
		Entries:              entries,
		StagingPath:          filepath.Join(root, "fs", "deletion-purge-staging", "identity-proof", operationToken),
		Phase:                sessionDeletionPurgePrepared,
		PreparedAt:           time.Now().UTC().Format(time.RFC3339Nano),
	}
}
