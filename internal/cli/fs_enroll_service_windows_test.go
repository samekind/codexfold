//go:build windows

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func TestWindowsEnrollmentSCMStopAndShutdown(t *testing.T) {
	for _, control := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		requests := make(chan svc.ChangeRequest, 1)
		changes := make(chan svc.Status, 8)
		stopped := make(chan uint32, 1)
		handler := &windowsEnrollmentService{log: io.Discard, run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
		go func() { _, code := handler.Execute(nil, requests, changes); stopped <- code }()
		if start := <-changes; start.State != svc.StartPending {
			t.Fatal(start)
		}
		if running := <-changes; running.State != svc.Running || running.Accepts&svc.AcceptShutdown == 0 {
			t.Fatal(running)
		}
		requests <- svc.ChangeRequest{Cmd: control}
		if stop := <-changes; stop.State != svc.StopPending {
			t.Fatal(stop)
		}
		select {
		case code := <-stopped:
			if code != 0 {
				t.Fatalf("stop code=%d", code)
			}
		case <-time.After(time.Second):
			t.Fatal("SCM control did not cancel worker")
		}
	}
}

func TestWindowsEnrollmentJobContainsChildren(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "child-pid")
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsEnrollmentJobHostHelper$", "-test.timeout=20s")
	command.Env = append(os.Environ(), "CODEXFOLD_TEST_JOB_HOST=1", "CODEXFOLD_TEST_JOB_READY="+ready)
	configureEnrollmentChild(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for pid == 0 {
		data, err := os.ReadFile(ready)
		if err == nil {
			pid, _ = strconv.Atoi(string(data))
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated job child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	defer windows.TerminateProcess(child, 1)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	result, err := windows.WaitForSingleObject(child, 5000)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("orphan child survived host crash: wait=%d err=%v", result, err)
	}
}

func TestWindowsEnrollmentJobHostHelper(t *testing.T) {
	if os.Getenv("CODEXFOLD_TEST_JOB_HOST") != "1" {
		t.Skip("isolated subprocess helper")
	}
	if err := containEnrollmentServiceChildren(); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestWindowsEnrollmentJobChildHelper$", "-test.timeout=20s")
	child.Env = append(os.Environ(), "CODEXFOLD_TEST_JOB_CHILD=1")
	configureEnrollmentChild(child)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("CODEXFOLD_TEST_JOB_READY"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
}

func TestWindowsEnrollmentJobChildHelper(t *testing.T) {
	if os.Getenv("CODEXFOLD_TEST_JOB_CHILD") != "1" {
		t.Skip("isolated subprocess helper")
	}
	time.Sleep(15 * time.Second)
}

func TestWindowsEnrollmentSCMUnexpectedReturnTriggersRecovery(t *testing.T) {
	handler := &windowsEnrollmentService{log: io.Discard, run: func(context.Context) error { return nil }}
	_, code := handler.Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 8))
	if code == 0 {
		t.Fatal("unexpected exit was reported as a user stop")
	}
}
