package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/samekind/codexfold/internal/service"
)

type rollbackTestApp struct {
	changed  bool
	rollback func() error
}

func (a *rollbackTestApp) AppGroupPath() string { return "/tmp/group.vip.jstar.codexfold" }
func (a *rollbackTestApp) Changed() bool        { return a.changed }
func (a *rollbackTestApp) Commit() error        { return nil }
func (a *rollbackTestApp) Rollback(context.Context) error {
	if a.rollback == nil {
		return nil
	}
	return a.rollback()
}

func TestRollbackFailedServiceInstallRestoresDefinitionAndAppBeforeRestart(t *testing.T) {
	root := t.TempDir()
	definition := filepath.Join(root, "com.codexfold.test.plist")
	binary := filepath.Join(root, "codexfold")
	candidate := filepath.Join(root, "candidate")
	if err := os.WriteFile(definition, []byte("old-definition"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("old-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("new-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	update, err := service.StageDefinitionUpdate(definition, []byte("new-definition"))
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Promote(); err != nil {
		t.Fatal(err)
	}
	binaryUpdate, err := service.StageBinaryUpdate(candidate, binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := binaryUpdate.Promote(); err != nil {
		t.Fatal(err)
	}

	var order []string
	appRolledBack := false
	app := &rollbackTestApp{changed: true, rollback: func() error {
		current, err := os.ReadFile(definition)
		if err != nil {
			return err
		}
		if string(current) != "old-definition" {
			return errors.New("app rollback ran before the service definition was restored")
		}
		order = append(order, "app-rollback")
		appRolledBack = true
		return nil
	}}
	stop := func(context.Context, service.Platform, string) error {
		order = append(order, "stop")
		return errors.New("already stopped")
	}
	start := func(context.Context, service.Platform, string, string) error {
		if !appRolledBack {
			return errors.New("service restarted before app rollback")
		}
		current, err := os.ReadFile(definition)
		if err != nil {
			return err
		}
		if string(current) != "old-definition" {
			return errors.New("service restarted before definition rollback")
		}
		current, err = os.ReadFile(binary)
		if err != nil {
			return err
		}
		if string(current) != "old-binary" {
			return errors.New("service restarted before binary rollback")
		}
		order = append(order, "start")
		return nil
	}

	if err := rollbackFailedServiceInstall(
		context.Background(), service.PlatformLaunchd, definition, filepath.Join(root, "mount"),
		[]*service.DefinitionUpdate{update}, app, binaryUpdate, true, stop, start,
	); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop", "app-rollback", "start"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("rollback order = %v, want %v", order, want)
	}
	artifacts, err := filepath.Glob(filepath.Join(root, ".codexfold-definition-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("definition rollback artifacts remain: %v", artifacts)
	}
}

func TestRollbackFailedFirstInstallDoesNotStartAService(t *testing.T) {
	root := t.TempDir()
	definition := filepath.Join(root, "com.codexfold.test.plist")
	update, err := service.StageDefinitionUpdate(definition, []byte("new-definition"))
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Promote(); err != nil {
		t.Fatal(err)
	}
	started := false
	if err := rollbackFailedServiceInstall(
		context.Background(), service.PlatformLaunchd, definition, filepath.Join(root, "mount"),
		[]*service.DefinitionUpdate{update}, nil, nil, false,
		func(context.Context, service.Platform, string) error { return nil },
		func(context.Context, service.Platform, string, string) error { started = true; return nil },
	); err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("failed first install restarted a service that did not previously exist")
	}
	if _, err := os.Stat(definition); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed first install left definition behind: %v", err)
	}
}

func TestValidateLaunchdChildProcessRequiresLockOwnerToBelongToHost(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd child ancestry is macOS-only")
	}
	lockPath := filepath.Join(t.TempDir(), "service.lock")
	lock, err := service.AcquireProcessLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	status := validateLaunchdChildProcess(service.Status{DaemonRunning: true, DaemonPID: os.Getppid()}, lockPath, "test")
	if !status.DaemonRunning || status.DaemonError != "" {
		t.Fatalf("valid child process was rejected: %#v", status)
	}
	status = validateLaunchdChildProcess(service.Status{DaemonRunning: true, DaemonPID: os.Getppid() + 1}, lockPath, "test")
	if status.DaemonRunning || status.DaemonError == "" {
		t.Fatalf("foreign child process was accepted: %#v", status)
	}
}

func TestNativeFSKitProcessLocksReportHeldOwners(t *testing.T) {
	root := t.TempDir()
	paths := nativeFSKitProcessLockPaths{
		daemon:     filepath.Join(root, "service.lock"),
		supervisor: filepath.Join(root, "supervisor.lock"),
	}
	daemon, err := service.AcquireProcessLock(paths.daemon)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	supervisor, err := service.AcquireProcessLock(paths.supervisor)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	daemonStatus, err := service.InspectProcessLock(paths.daemon)
	if err != nil {
		t.Fatal(err)
	}
	supervisorStatus, err := service.InspectProcessLock(paths.supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if !daemonStatus.Held || daemonStatus.PID != os.Getpid() {
		t.Fatalf("daemon lock = %#v", daemonStatus)
	}
	if !supervisorStatus.Held || supervisorStatus.PID != os.Getpid() {
		t.Fatalf("supervisor lock = %#v", supervisorStatus)
	}
}

func TestNativeFSKitServiceInactiveRequiresUnmountedMount(t *testing.T) {
	status := service.Status{}
	if nativeFSKitServiceInactive(status, service.ProcessLockStatus{}, service.ProcessLockStatus{}, true, nil) {
		t.Fatal("an unhealthy but still-mounted filesystem was accepted as inactive")
	}
	if nativeFSKitServiceInactive(status, service.ProcessLockStatus{}, service.ProcessLockStatus{}, false, errors.New("mount state unknown")) {
		t.Fatal("an unknown mount state was accepted as inactive")
	}
	if !nativeFSKitServiceInactive(status, service.ProcessLockStatus{}, service.ProcessLockStatus{}, false, nil) {
		t.Fatal("fully stopped native FSKit service was not accepted as inactive")
	}
}
