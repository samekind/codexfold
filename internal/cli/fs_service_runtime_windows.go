//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jstar0/codexfold/internal/mountfs"
	"github.com/jstar0/codexfold/internal/service"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows/svc"
)

func addPlatformServiceCommands(parent *cobra.Command) {
	parent.AddCommand(newFSServiceRunCommand())
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
			handler := &windowsFSService{
				log: stderr,
				run: func(ctx context.Context) error {
					serve := newFSServeCommand()
					serve.SetArgs(config.Arguments[2:])
					serve.SetOut(stdout)
					serve.SetErr(stderr)
					serve.SilenceErrors = true
					serve.SilenceUsage = true
					return serve.ExecuteContext(ctx)
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
