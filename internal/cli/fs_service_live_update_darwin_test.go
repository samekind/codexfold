//go:build darwin

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
)

func TestLiveDaemonCandidateProtocolRejectsImmediateExit(t *testing.T) {
	root := t.TempDir()
	for name, source := range map[string]string{
		"failed": "#!/bin/sh\nexit 9\n",
		"silent": "#!/bin/sh\nexit 0\n",
	} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(source), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := requireLiveDaemonCandidateProtocol(context.Background(), path); err == nil {
			t.Fatalf("%s was accepted as a filesystem backend", name)
		}
	}
	good := filepath.Join(root, "good")
	if err := os.WriteFile(good, []byte("#!/bin/sh\nprintf '%s\\n' --canonical-namespace --frontend --fskit-resource --fskit-socket\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := requireLiveDaemonCandidateProtocol(context.Background(), good); err != nil {
		t.Fatal(err)
	}
}

func TestLiveDaemonBinaryUpdatePromotesWithoutStoppingOtherComponents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "installed")
	candidate := filepath.Join(root, "candidate")
	for path, value := range map[string]string{target: "old", candidate: "new"} {
		if err := os.WriteFile(path, []byte(value), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	err := replaceDaemonBinaryLive(candidate, target, func(string) error {
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "new" {
			t.Fatalf("candidate was not installed before restart: %q %v", data, err)
		}
		calls = append(calls, "restart-daemon")
		return nil
	}, func(string) error {
		t.Fatal("healthy replacement must not roll back")
		return nil
	})
	if err != nil || !reflect.DeepEqual(calls, []string{"restart-daemon"}) {
		t.Fatalf("live update calls=%v err=%v", calls, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new" {
		t.Fatalf("installed binary=%q err=%v", data, err)
	}
}

func TestLiveDaemonBinaryUpdateRestoresOldBeforeRestartOnFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "installed")
	candidate := filepath.Join(root, "candidate")
	for path, value := range map[string]string{target: "old", candidate: "new"} {
		if err := os.WriteFile(path, []byte(value), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	err := replaceDaemonBinaryLive(candidate, target, func(string) error {
		calls = append(calls, "restart-new")
		return errors.New("replacement did not start")
	}, func(string) error {
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "old" {
			t.Fatalf("rollback did not restore old binary first: %q %v", data, err)
		}
		calls = append(calls, "restart-old")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "replacement did not start") || !reflect.DeepEqual(calls, []string{"restart-new", "restart-old"}) {
		t.Fatalf("live rollback calls=%v err=%v", calls, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "old" {
		t.Fatalf("installed binary after rollback=%q err=%v", data, err)
	}
}

func TestLiveDaemonUpdateRefusesToInterruptBackgroundMutation(t *testing.T) {
	store := t.TempDir()
	if err := requireIdleEnrollmentForLiveUpdate(store); err == nil {
		t.Fatal("missing progress was accepted as an idle service")
	}
	path := enroll.ProgressPath(store)
	for _, phase := range []string{enroll.PhasePacking, enroll.PhaseReclaiming, enroll.PhaseIdle} {
		if err := enroll.SaveProgress(path, enroll.Progress{Phase: phase, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		err := requireIdleEnrollmentForLiveUpdate(store)
		if (err == nil) != (phase == enroll.PhaseIdle) {
			t.Fatalf("phase=%s error=%v", phase, err)
		}
	}
}
