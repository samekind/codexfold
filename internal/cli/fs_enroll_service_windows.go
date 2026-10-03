//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/samekind/codexfold/internal/service"
	"github.com/samekind/codexfold/internal/sessionns"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func addPlatformEnrollmentCommands(parent *cobra.Command) {
	command := &cobra.Command{Use: "service", Short: "Run the persistent Windows enrollment service"}
	command.AddCommand(newEnrollmentServiceCommand(false), newEnrollmentServiceCommand(true))
	command.AddCommand(newEnrollmentServiceInstallCommand())
	parent.AddCommand(command)
}

func newEnrollmentServiceCommand(validateOnly bool) *cobra.Command {
	var definition string
	name := "run"
	if validateOnly {
		name = "validate"
	}
	command := &cobra.Command{Use: name, Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		if !filepath.IsAbs(definition) {
			return errors.New("absolute enrollment service binding path is required")
		}
		data, err := os.ReadFile(definition)
		if err != nil {
			return err
		}
		config, err := service.ParseWindowsEnrollmentConfig(data)
		if err != nil {
			return err
		}
		if _, err := windows.StringToSid(config.OwnerSID); err != nil {
			return fmt.Errorf("invalid enrollment owner SID: %w", err)
		}
		if validateOnly {
			return writeJSON(command, config)
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		if !strings.EqualFold(filepath.Clean(binary), filepath.Clean(config.BinaryPath)) {
			return errors.New("enrollment binding binary does not match this executable")
		}
		isService, err := svc.IsWindowsService()
		if err != nil {
			return err
		}
		if !isService {
			return errors.New("enrollment service must be started by the Service Control Manager")
		}
		if err := containEnrollmentServiceChildren(); err != nil {
			return err
		}
		stdout, stderr, closeLogs, err := openWindowsServiceLogs(service.WindowsConfig{StdoutPath: config.StdoutPath, StderrPath: config.StderrPath})
		if err != nil {
			return err
		}
		defer closeLogs()
		if err := windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS); err != nil {
			return err
		}
		flags := enrollmentFlags{codexHome: config.CodexHome, storeDir: config.Store, mountPoint: config.Mount, nativeRoot: config.NativeRoot, canonicalNamespace: true, stableFor: time.Hour, batchSize: 1, externalWorker: true}
		flags.filesystemReady = func() error {
			if err := service.ProbeMount(config.Mount); err != nil {
				return err
			}
			state, err := sessionns.Inspect(sessionns.Options{Home: config.CodexHome, Mount: config.Mount, NativeRoot: config.NativeRoot})
			if err != nil {
				return err
			}
			if !state.Active {
				return errors.New("the bound canonical namespace is not active")
			}
			return nil
		}
		handler := &windowsEnrollmentService{log: stderr, run: func(ctx context.Context) error { return runEnrollmentWorker(ctx, flags, true) }}
		_ = stdout
		return svc.Run(config.ServiceName, handler)
	}}
	command.Flags().StringVar(&definition, "definition", "", "Administrator-owned enrollment service binding")
	return command
}

// Children inherit this job. If SCM restarts a crashed host, its interrupted
// pack/migrate children cannot keep mutating the store alongside the new host.
// The host deliberately retains the non-inheritable handle until process exit:
// closing it here would terminate the host itself before reporting SCM status.
func containEnrollmentServiceChildren() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return err
	}
	return nil
}

type windowsEnrollmentService struct {
	run func(context.Context) error
	log io.Writer
}

func (s *windowsEnrollmentService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes <- svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 15000}
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	running := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	changes <- running
	for {
		select {
		case err := <-done:
			// A normal user pause never exits the persistent loop. An unexpected
			// return must trigger SCM recovery, including a nil return.
			_, _ = fmt.Fprintf(s.log, "enrollment service exited unexpectedly: %v\n", err)
			return false, 1
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
						_, _ = fmt.Fprintf(s.log, "enrollment shutdown failed: %v\n", err)
						return false, 1
					}
					return false, 0
				case <-time.After(30 * time.Second):
					_, _ = fmt.Fprintln(s.log, "enrollment shutdown timed out")
					return false, 1
				}
			}
		}
	}
}
