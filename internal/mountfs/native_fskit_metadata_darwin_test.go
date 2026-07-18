//go:build darwin

package mountfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/fskitproto"
)

func TestNativeFSKitServerPersistsMetadataAndExtendedAttributes(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	directory := filepath.Join(nativeRoot, "sessions", "2026", "07", "17")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(nativeRoot, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "session.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	client, stop := startNativeFSKitTestServer(t, filesystem, root)
	defer stop()
	defer client.Close()

	path := "/sessions/2026/07/17/session.jsonl"
	atime := time.Unix(1_720_000_000, 123_000_000)
	mtime := time.Unix(1_720_000_100, 456_000_000)
	setattr := fskitproto.NewEncoder(160)
	setattr.String(path)
	setattr.Uint32(fskitproto.SetAttrMode | fskitproto.SetAttrUID | fskitproto.SetAttrGID | fskitproto.SetAttrAccessTime | fskitproto.SetAttrModifyTime)
	setattr.Uint32(0o640)
	setattr.Uint32(uint32(os.Getuid()))
	setattr.Uint32(uint32(os.Getgid()))
	setattr.Time(atime)
	setattr.Time(mtime)
	if _, err := client.Call(fskitproto.OpSetattr, setattr.Data()); err != nil {
		t.Fatalf("setattr: %v", err)
	}

	getattr := fskitproto.NewEncoder(128)
	getattr.String(path)
	response, err := client.Call(fskitproto.OpGetattr, getattr.Data())
	if err != nil {
		t.Fatal(err)
	}
	decoder := fskitproto.NewDecoder(response)
	entry, err := decoder.Entry()
	if err != nil || decoder.Done() != nil {
		t.Fatalf("decode getattr: %v", err)
	}
	if entry.Mode != 0o640 || entry.UID != uint32(os.Getuid()) || entry.GID != uint32(os.Getgid()) {
		t.Fatalf("metadata = mode %#o uid %d gid %d", entry.Mode, entry.UID, entry.GID)
	}
	if !entry.AccessTime.Equal(atime) || !entry.ModTime.Equal(mtime) {
		t.Fatalf("times = atime %s mtime %s, want %s and %s", entry.AccessTime, entry.ModTime, atime, mtime)
	}

	attribute := "vip.jstar.codexfold.test"
	value := []byte("first")
	setXattr := fskitproto.NewEncoder(160)
	setXattr.String(path)
	setXattr.String(attribute)
	setXattr.Uint32(uint32(fskitproto.XattrAlwaysSet))
	setXattr.Bytes(value)
	if _, err := client.Call(fskitproto.OpSetXattr, setXattr.Data()); err != nil {
		t.Fatalf("set xattr: %v", err)
	}

	getXattr := fskitproto.NewEncoder(160)
	getXattr.String(path)
	getXattr.String(attribute)
	response, err = client.Call(fskitproto.OpGetXattr, getXattr.Data())
	if err != nil {
		t.Fatalf("get xattr: %v", err)
	}
	decoder = fskitproto.NewDecoder(response)
	got, err := decoder.Bytes(1024)
	if err != nil || decoder.Done() != nil || !bytes.Equal(got, value) {
		t.Fatalf("xattr value = %q err=%v", got, err)
	}

	createAgain := fskitproto.NewEncoder(160)
	createAgain.String(path)
	createAgain.String(attribute)
	createAgain.Uint32(uint32(fskitproto.XattrMustCreate))
	createAgain.Bytes([]byte("duplicate"))
	if _, err := client.Call(fskitproto.OpSetXattr, createAgain.Data()); fskitproto.ErrorNumber(err) != syscall.EEXIST {
		t.Fatalf("must-create error = %v, want EEXIST", err)
	}

	list := fskitproto.NewEncoder(128)
	list.String(path)
	response, err = client.Call(fskitproto.OpListXattrs, list.Data())
	if err != nil {
		t.Fatalf("list xattrs: %v", err)
	}
	decoder = fskitproto.NewDecoder(response)
	count, err := decoder.Uint32()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, count)
	for range count {
		name, decodeErr := decoder.String(4096)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		names = append(names, name)
	}
	if err := decoder.Done(); err != nil || !slices.Contains(names, attribute) {
		t.Fatalf("xattr names = %v err=%v", names, err)
	}

	remove := fskitproto.NewEncoder(160)
	remove.String(path)
	remove.String(attribute)
	remove.Uint32(uint32(fskitproto.XattrDelete))
	remove.Bytes(nil)
	if _, err := client.Call(fskitproto.OpSetXattr, remove.Data()); err != nil {
		t.Fatalf("remove xattr: %v", err)
	}
	if _, err := client.Call(fskitproto.OpGetXattr, getXattr.Data()); !errors.Is(err, fskitproto.StatusError{Operation: fskitproto.OpGetXattr, Errno: syscall.ENOATTR}) && fskitproto.ErrorNumber(err) != syscall.ENOATTR {
		t.Fatalf("removed xattr error = %v, want ENOATTR", err)
	}
}
