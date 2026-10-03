//go:build windows

package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsEnrollmentBindingRejectsAmbiguousRoots(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	valid := WindowsEnrollmentConfig{Version: 1, ServiceName: WindowsEnrollmentServiceName, BinaryPath: filepath.Join(home, "bin.exe"), OwnerSID: "S-1-5-21-1-2-3-1001", CodexHome: home, Store: filepath.Join(home, "fold-store"), NativeRoot: filepath.Join(home, "fold-native"), Mount: `V:\`, StdoutPath: filepath.Join(home, "out.log"), StderrPath: filepath.Join(home, "err.log")}
	for _, mutate := range []func(*WindowsEnrollmentConfig){nil,
		func(c *WindowsEnrollmentConfig) { c.CodexHome = "" },
		func(c *WindowsEnrollmentConfig) { c.Store = filepath.Join(home, "other-store") },
		func(c *WindowsEnrollmentConfig) { c.NativeRoot = filepath.Dir(home) },
		func(c *WindowsEnrollmentConfig) { c.OwnerSID = "" },
		func(c *WindowsEnrollmentConfig) { c.ServiceName = "unrelated" },
		func(c *WindowsEnrollmentConfig) { c.Version = 2 },
	} {
		candidate := valid
		if mutate != nil {
			mutate(&candidate)
		}
		data, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParseWindowsEnrollmentConfig(data)
		if (err == nil) != (mutate == nil) {
			t.Fatalf("binding %#v: %v", candidate, err)
		}
	}
}

func TestWindowsEnrollmentManagerRegistersRecoveryWithoutFilesystemRestart(t *testing.T) {
	runner := &recordingRunner{errors: map[string]error{"sc.exe query " + WindowsEnrollmentServiceName: errors.New("not installed")}}
	manager := WindowsManager{Runner: runner}
	if err := manager.InstallEnrollment(context.Background(), WindowsEnrollmentServiceName, `C:\Program Files\CodexFold\Enrollment\codexfold-enroll.exe`, `C:\ProgramData\CodexFold\Enrollment\service.json`); err != nil {
		t.Fatal(err)
	}
	actual := strings.Join(runner.calls, "\n")
	for _, want := range []string{"fs enroll service run --definition", "start= delayed-auto", "actions= restart/5000/restart/15000/restart/60000", "failureflag " + WindowsEnrollmentServiceName + " 1"} {
		if !strings.Contains(actual, want) {
			t.Fatalf("missing %s: %s", want, actual)
		}
	}
	if strings.Contains(actual, "com.codexfold.fs") {
		t.Fatal("enrollment registration touched filesystem service")
	}
}
