//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/service"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func addPlatformServiceCommands(parent *cobra.Command) {
	parent.AddCommand(newFSServiceRunCommand())
	parent.AddCommand(newFSServiceChildCommand())
}

func newFSServiceChildCommand() *cobra.Command {
	var definitionPath string
	command := &cobra.Command{
		Use: "child", Hidden: true, Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !filepath.IsAbs(definitionPath) {
				return errors.New("absolute Windows service definition path is required")
			}
			definition, err := os.ReadFile(filepath.Clean(definitionPath))
			if err != nil {
				return err
			}
			config, err := service.ParseWindowsConfig(definition)
			if err != nil {
				return err
			}
			if config.ServiceName != serviceLabel {
				return errors.New("Windows service definition name does not match this binary")
			}
			if err := os.Setenv("CODEXFOLD_WINDOWS_GLOBAL_MOUNT", "1"); err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(command.Context())
			defer cancel()
			// The SCM parent owns this pipe. Closing it, including if the
			// parent unexpectedly exits, requests a normal WinFsp unmount.
			go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
			serve := newFSServeCommand()
			arguments := append([]string(nil), config.Arguments[2:]...)
			arguments = append(arguments, "--frontend=windows-proxy")
			serve.SetArgs(arguments)
			serve.SetOut(command.OutOrStdout())
			serve.SetErr(command.ErrOrStderr())
			serve.SilenceErrors, serve.SilenceUsage = true, true
			err = serve.ExecuteContext(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	command.Flags().StringVar(&definitionPath, "definition", "", "Absolute Windows service definition path")
	return command
}

func runWindowsServiceChild(ctx context.Context, config service.WindowsConfig, definitionPath string, stdout, stderr io.Writer) error {
	// WinFsp's FUSE loop calls FspServiceRun internally. It cannot run in
	// the process already hosting Go's svc.Run (ERROR_SERVICE_ALREADY_RUNNING).
	child := exec.Command(config.BinaryPath, "fs", "service", "child", "--definition", definitionPath)
	child.Stdout, child.Stderr = stdout, stderr
	child.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	control, err := child.StdinPipe()
	if err != nil {
		return err
	}
	defer control.Close()
	if err := child.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err := <-done:
		if err == nil && ctx.Err() == nil {
			return errors.New("Windows filesystem child exited unexpectedly")
		}
		return err
	case <-ctx.Done():
		_ = control.Close()
		select {
		case err := <-done:
			return err
		case <-time.After(25 * time.Second):
			_ = child.Process.Kill()
			<-done
			return errors.New("Windows filesystem child did not finish unmounting")
		}
	}
}

func newFSServiceRunCommand() *cobra.Command {
	var definitionPath string
	command := &cobra.Command{
		Use:    "run",
		Short:  "Run the Windows SCM service host",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !mountfs.Available() {
				return errors.New("Windows service runtime requires a WinFsp-enabled build")
			}
			if !filepath.IsAbs(definitionPath) {
				return errors.New("absolute Windows service definition path is required")
			}
			definition, err := os.ReadFile(filepath.Clean(definitionPath))
			if err != nil {
				return err
			}
			config, err := service.ParseWindowsConfig(definition)
			if err != nil {
				return err
			}
			if config.ServiceName != serviceLabel {
				return errors.New("Windows service definition name does not match this binary")
			}
			isService, err := svc.IsWindowsService()
			if err != nil {
				return err
			}
			if !isService {
				return errors.New("Windows service run must be started by the Service Control Manager")
			}
			stdout, stderr, closeLogs, err := openWindowsServiceLogs(config)
			if err != nil {
				return err
			}
			defer closeLogs()
			// WinFsp writes startup diagnostics through the process standard
			// handles, independently of Cobra's writers. SCM supplies no console.
			os.Stdout = stdout.(*os.File)
			os.Stderr = stderr.(*os.File)
			if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(os.Stdout.Fd())); err != nil {
				return err
			}
			if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(os.Stderr.Fd())); err != nil {
				return err
			}
			handler := &windowsFSService{
				log: stderr,
				run: func(ctx context.Context) error {
					return runWindowsServiceChild(ctx, config, definitionPath, stdout, stderr)
				},
			}
			return svc.Run(config.ServiceName, handler)
		},
	}
	command.Flags().StringVar(&definitionPath, "definition", "", "Absolute Windows service definition path")
	return command
}

type windowsFSService struct {
	run func(context.Context) error
	log io.Writer
}

func (s *windowsFSService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	changes <- svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 15000}
	_, _ = fmt.Fprintln(s.log, "starting Windows filesystem service")
	go func() { done <- s.run(ctx) }()
	running := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	changes <- running

	for {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintf(s.log, "filesystem service exited: %v\n", err)
				return false, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- running
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 30000}
				cancel()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						_, _ = fmt.Fprintf(s.log, "filesystem service shutdown failed: %v\n", err)
						return false, 1
					}
					return false, 0
				case <-time.After(30 * time.Second):
					_, _ = fmt.Fprintln(s.log, "filesystem service shutdown timed out")
					return false, 1
				}
			}
		}
	}
}

func openWindowsServiceLogs(config service.WindowsConfig) (io.Writer, io.Writer, func(), error) {
	for _, path := range []string{config.StdoutPath, config.StderrPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, nil, nil, err
		}
	}
	stdout, err := os.OpenFile(config.StdoutPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := os.OpenFile(config.StderrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, nil, err
	}
	closeLogs := func() {
		_ = stdout.Sync()
		_ = stderr.Sync()
		_ = stdout.Close()
		_ = stderr.Close()
	}
	return stdout, stderr, closeLogs, nil
}
