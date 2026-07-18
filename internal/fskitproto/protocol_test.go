package fskitproto

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	want := Frame{Kind: KindRequest, Op: OpWrite, Flags: 7, RequestID: 99, Generation: 42, Status: -5, Payload: []byte("payload")}
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, want, 0); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buffer, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("frame = %#v, want %#v", got, want)
	}
}

func TestEntryRoundTrip(t *testing.T) {
	want := Entry{
		Path: "/sessions/2026/07/session.jsonl", Name: "session.jsonl", NodeID: 9, ParentID: 8,
		Type: EntryFile, Mode: 0o600, UID: 501, GID: 20, Size: 1234, AllocSize: 4096,
		ModTime: time.Unix(1_700_000_000, 123), ChangeTime: time.Unix(1_700_000_001, 456),
		AccessTime: time.Unix(1_700_000_002, 789), NamespaceID: 33,
	}
	encoder := NewEncoder(256)
	encoder.Entry(want)
	decoder := NewDecoder(encoder.Data())
	got, err := decoder.Entry()
	if err != nil {
		t.Fatal(err)
	}
	if err := decoder.Done(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entry = %#v, want %#v", got, want)
	}
}

func TestDescriptorRoundTrip(t *testing.T) {
	want := Descriptor{Generation: 17, SocketPath: "/tmp/codexfold.sock", Token: bytes.Repeat([]byte{0x5a}, 32)}
	encoded, err := EncodeDescriptor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeDescriptor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descriptor = %#v, want %#v", got, want)
	}
}

func TestResourceDescriptorPathSupportsSecurityScopedDirectoryAndLegacyFile(t *testing.T) {
	root := t.TempDir()
	directoryResource := filepath.Join(root, "native-fskit")
	if err := os.Mkdir(directoryResource, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := ResourceDescriptorPath(directoryResource)
	if err != nil || path != filepath.Join(directoryResource, DescriptorFilename) {
		t.Fatalf("directory descriptor = %q err=%v", path, err)
	}
	legacy := filepath.Join(root, "resource.bin")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err = ResourceDescriptorPath(legacy)
	if err != nil || path != legacy {
		t.Fatalf("legacy descriptor = %q err=%v", path, err)
	}
}

func TestReadFrameRejectsOversizedPayloadBeforeAllocation(t *testing.T) {
	frame := Frame{Kind: KindRequest, Op: OpWrite, Payload: bytes.Repeat([]byte("x"), 64)}
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, frame, 128); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrame(&buffer, 32); err == nil {
		t.Fatal("ReadFrame unexpectedly accepted an oversized payload")
	}
}
