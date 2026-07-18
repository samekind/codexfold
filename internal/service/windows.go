package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const windowsConfigVersion = 1

var windowsRunningState = regexp.MustCompile(`(?m)STATE\s*:\s*4\s+RUNNING\b`)

type WindowsConfig struct {
	Version     int      `json:"version"`
	ServiceName string   `json:"service_name"`
	BinaryPath  string   `json:"binary_path,omitempty"`
	Arguments   []string `json:"arguments"`
	StdoutPath  string   `json:"stdout_path"`
	StderrPath  string   `json:"stderr_path"`
}

type WindowsManager struct {
	Runner     Runner
	MountProbe func(string) error
}

func RenderWindowsConfig(options Options) ([]byte, error) {
	arguments, err := ServeArguments(options)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(WindowsConfig{
		Version: windowsConfigVersion, ServiceName: options.Label, BinaryPath: options.BinaryPath, Arguments: arguments,
		StdoutPath: options.StdoutPath, StderrPath: options.StderrPath,
	}, "", "  ")
}

func ParseWindowsConfig(definition []byte) (WindowsConfig, error) {
	var config WindowsConfig
	if err := json.Unmarshal(definition, &config); err != nil {
		return WindowsConfig{}, err
	}
	if config.Version != windowsConfigVersion {
		return WindowsConfig{}, fmt.Errorf("unsupported Windows service config version %d", config.Version)
	}
	if !safeLabel(config.ServiceName) {
		return WindowsConfig{}, errors.New("safe Windows service name is required")
	}
	if config.BinaryPath == "" || (!filepath.IsAbs(config.BinaryPath) && !absoluteWindowsServicePath(config.BinaryPath)) {
		return WindowsConfig{}, errors.New("Windows service config binary path must be absolute")
	}
	if len(config.Arguments) < 2 || config.Arguments[0] != "fs" || config.Arguments[1] != "serve" {
		return WindowsConfig{}, errors.New("Windows service config must run fs serve")
	}
	for _, path := range []string{config.StdoutPath, config.StderrPath} {
		if !filepath.IsAbs(path) {
			return WindowsConfig{}, errors.New("Windows service log paths must be absolute")
		}
	}
	return config, nil
}

func (m WindowsManager) Install(ctx context.Context, name string, binaryPath string, definitionPath string) error {
	if !safeLabel(name) {
		return errors.New("safe Windows service name is required")
	}
	if !absoluteWindowsServicePath(binaryPath) || !absoluteWindowsServicePath(definitionPath) {
		return errors.New("Windows service binary and definition paths must be absolute")
	}
	commandLine := windowsServiceCommand(binaryPath, definitionPath)
	_, queryErr := m.runner().Run(ctx, "sc.exe", "query", name)
	if queryErr == nil {
		output, err := m.runner().Run(ctx, "sc.exe", "config", name, "binPath=", commandLine, "start=", "auto")
		if err != nil {
			return commandFailure("sc.exe config", output, err)
		}
	} else {
		output, err := m.runner().Run(ctx, "sc.exe", "create", name, "binPath=", commandLine, "start=", "auto", "DisplayName=", "CodexFold Transparent Session Filesystem")
		if err != nil {
			return commandFailure("sc.exe create", output, err)
		}
	}
	if output, err := m.runner().Run(ctx, "sc.exe", "description", name, "CodexFold transparent Codex session filesystem"); err != nil {
		return commandFailure("sc.exe description", output, err)
	}
	if output, err := m.runner().Run(ctx, "sc.exe", "failure", name, "reset=", "86400", "actions=", "restart/5000/restart/15000/\"\"/0"); err != nil {
		return commandFailure("sc.exe failure", output, err)
	}
	return nil
}

func (m WindowsManager) Start(ctx context.Context, name string) error {
	if !safeLabel(name) {
		return errors.New("safe Windows service name is required")
	}
	output, err := m.runner().Run(ctx, "sc.exe", "start", name)
	if err != nil {
		return commandFailure("sc.exe start", output, err)
	}
	return nil
}

func (m WindowsManager) Stop(ctx context.Context, name string) error {
	if !safeLabel(name) {
		return errors.New("safe Windows service name is required")
	}
	output, err := m.runner().Run(ctx, "sc.exe", "stop", name)
	if err != nil {
		return commandFailure("sc.exe stop", output, err)
	}
	return nil
}

func (m WindowsManager) Status(ctx context.Context, name string, mountPoint string) Status {
	result := Status{}
	if !safeLabel(name) {
		result.DaemonError = "safe Windows service name is required"
	} else {
		output, err := m.runner().Run(ctx, "sc.exe", "queryex", name)
		if err != nil {
			result.DaemonError = commandFailure("sc.exe queryex", output, err).Error()
		} else if windowsRunningState.Match(output) {
			result.DaemonRunning = true
		} else {
			result.DaemonError = "Windows service is installed but not running"
		}
	}
	probe := m.MountProbe
	if probe == nil {
		probe = ProbeMount
	}
	if err := probe(mountPoint); err != nil {
		result.MountError = err.Error()
	} else {
		result.MountHealthy = true
	}
	return result
}

func (m WindowsManager) WaitHealthy(ctx context.Context, name string, mountPoint string, timeout time.Duration) (Status, error) {
	return waitHealthy(ctx, timeout, func() Status { return m.Status(ctx, name, mountPoint) })
}

func (m WindowsManager) runner() Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return ExecRunner{}
}

func windowsServiceCommand(binaryPath string, definitionPath string) string {
	return strings.Join([]string{
		quoteWindowsCommandLineArgument(binaryPath), "fs", "service", "run", "--definition",
		quoteWindowsCommandLineArgument(definitionPath),
	}, " ")
}

func quoteWindowsCommandLineArgument(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n\v\"") {
		return value
	}
	var output strings.Builder
	output.WriteByte('"')
	backslashes := 0
	for _, character := range value {
		switch character {
		case '\\':
			backslashes++
		case '"':
			output.WriteString(strings.Repeat("\\", backslashes*2+1))
			output.WriteRune(character)
			backslashes = 0
		default:
			output.WriteString(strings.Repeat("\\", backslashes))
			output.WriteRune(character)
			backslashes = 0
		}
	}
	output.WriteString(strings.Repeat("\\", backslashes*2))
	output.WriteByte('"')
	return output.String()
}

func absoluteWindowsServicePath(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if len(path) >= 3 && ((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		return true
	}
	return strings.HasPrefix(path, `\\`)
}
