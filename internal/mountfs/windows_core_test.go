//go:build windows

package mountfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsCoreEngineProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--core-fixture" {
		return
	}
	root := os.Args[len(os.Args)-1]
	boot, err := ReadWindowsCoreBoot(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	err = ServeWindowsCore(ctx, filesystem, boot, nil)
	_ = filesystem.CloseSessions()
	if err != nil && !errors.Is(err, context.Canceled) {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestWindowsCoreUpdatePreservesDataAndRejectsOpenHandles(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "native")
	if err := os.MkdirAll(filepath.Join(native, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	binary, _ := os.Executable()
	candidate := filepath.Join(root, "candidate.exe")
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, append(data, []byte("core-update-candidate")...), 0700); err != nil {
		t.Fatal(err)
	}
	paused := true
	options := WindowsCoreOptions{Binary: binary, Root: filepath.Join(root, "images"), Store: root,
		Arguments: []string{"-test.run=TestWindowsCoreEngineProcess", "--", "--core-fixture", native}, ReadyTimeout: 3 * time.Second,
		BeforeUpdate: func() error {
			if !paused {
				return errors.New("worker active")
			}
			return nil
		}}
	host, err := NewWindowsCoreHost(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ctx := context.Background()
	stop, err := host.ServeControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var before CoreInfo
	if err := CallWindowsCoreControl(ctx, root, "Info", struct{}{}, &before); err != nil {
		t.Fatal(err)
	}
	handle, errno := host.Open("/sessions/note.txt", os.O_CREATE|os.O_RDWR)
	if errno != 0 {
		t.Fatal(errno)
	}
	if count, errno := host.Write(handle, []byte("verified payload"), 0); errno != 0 || count != 16 {
		t.Fatalf("write %d %v", count, errno)
	}
	if result, err := host.Update(ctx, CoreUpdateRequest{Candidate: candidate, Apply: true}); err == nil || result.OpenHandles != 1 {
		t.Fatalf("open-handle guard: %+v %v", result, err)
	}
	if host.process.command.Process.Pid != before.EnginePID {
		t.Fatal("busy update replaced engine")
	}
	if errno := host.Fsync(handle); errno != 0 {
		t.Fatal(errno)
	}
	if errno := host.Release(handle); errno != 0 {
		t.Fatal(errno)
	}
	paused = false
	if _, err := host.Update(ctx, CoreUpdateRequest{Candidate: candidate, Apply: true}); err == nil || !strings.Contains(err.Error(), "worker active") {
		t.Fatal("worker guard", err)
	}
	paused = true
	result, err := host.Update(ctx, CoreUpdateRequest{Candidate: candidate, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.MountPreserved || result.ReplacementPID == before.EnginePID || result.HostPID != before.HostPID {
		t.Fatalf("cutover %+v", result)
	}
	handle, errno = host.Open("/sessions/note.txt", os.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	buffer := make([]byte, 100)
	count, errno := host.Read(handle, buffer, 0)
	if errno != 0 || string(buffer[:count]) != "verified payload" {
		t.Fatal("payload changed", errno)
	}
	if errno := host.Release(handle); errno != 0 {
		t.Fatal(errno)
	}
	// An invalid candidate must roll back to the last verified engine without
	// fabricating a successful update or replaying any writes.
	bad := filepath.Join(root, "broken.exe")
	if err := os.WriteFile(bad, []byte("bad executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Update(ctx, CoreUpdateRequest{Candidate: bad, Apply: true}); err == nil {
		t.Fatal("invalid executable accepted")
	}
	if !host.Healthy() || host.BuildSHA256() != result.Build {
		t.Fatal("rollback failed")
	}
	if _, errno := host.Getattr("/sessions/note.txt"); errno != 0 {
		t.Fatal(errno)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	// The committed image survives a restart of the resident host.
	restarted, err := NewWindowsCoreHost(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.BuildSHA256() != result.Build {
		t.Fatal("committed image was not restored")
	}
}
