//go:build windows

package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsDeletionObjectRejectsAlternateStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureSessionDeletionXattrs(file, info, path); err != nil {
		t.Fatal(err)
	}
	object, err := sessionDeletionObjectFromFile(file, info)
	if err != nil || !validSessionDeletionPurgeObject(object) {
		t.Fatalf("invalid NTFS proof: %+v, %v", object, err)
	}
	if err := os.WriteFile(path+":extra", []byte("keep this"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureSessionDeletionXattrs(file, info, path); err == nil {
		t.Fatal("alternate stream was eligible for purge")
	}
}

func TestWindowsDeletionObjectCapturesPermissionChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	before, err := sessionDeletionObjectFromFile(file, info)
	if err != nil {
		t.Fatal(err)
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + tokenUser.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	after, err := sessionDeletionObjectFromFile(file, info)
	if err != nil {
		t.Fatal(err)
	}
	if before.Inode != after.Inode || before.SecuritySHA256 == after.SecuritySHA256 {
		t.Fatal("changed ACL was not distinguished from the original deletion object")
	}
}
