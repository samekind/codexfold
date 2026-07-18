package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type SystemdManager struct {
	Runner     Runner
	MountProbe func(string) error
}

func RenderSystemd(options Options) ([]byte, error) {
	arguments, err := ServeArguments(options)
	if err != nil {
		return nil, err
	}
	execStart := make([]string, 0, len(arguments)+1)
	for _, argument := range append([]string{options.BinaryPath}, arguments...) {
		quoted, err := quoteSystemdArgument(argument)
		if err != nil {
			return nil, err
		}
		execStart = append(execStart, quoted)
	}
	stdout, err := escapeSystemdSettingValue("append:" + options.StdoutPath)
	if err != nil {
		return nil, err
	}
	stderr, err := escapeSystemdSettingValue("append:" + options.StderrPath)
	if err != nil {
		return nil, err
	}

	var output bytes.Buffer
	output.WriteString("[Unit]\n")
	output.WriteString("Description=CodexFold transparent session filesystem\n")
	output.WriteString("After=default.target\n\n")
	output.WriteString("[Service]\n")
	output.WriteString("Type=simple\n")
	output.WriteString("ExecStart=:")
	output.WriteString(strings.Join(execStart, " "))
	output.WriteByte('\n')
	output.WriteString("Restart=on-failure\n")
	output.WriteString("RestartSec=2s\n")
	output.WriteString("TimeoutStopSec=30s\n")
	output.WriteString("KillMode=mixed\n")
	output.WriteString("StandardOutput=")
	output.WriteString(stdout)
	output.WriteByte('\n')
	output.WriteString("StandardError=")
	output.WriteString(stderr)
	output.WriteString("\n\n[Install]\n")
	output.WriteString("WantedBy=default.target\n")
	return output.Bytes(), nil
}

func SystemdUnitName(label string) (string, error) {
	if !safeLabel(label) {
		return "", errors.New("safe service label is required")
	}
	return label + ".service", nil
}

func (m SystemdManager) Start(ctx context.Context, unit string) error {
	if !safeSystemdUnit(unit) {
		return errors.New("safe systemd service unit is required")
	}
	if output, err := m.runner().Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return commandFailure("systemctl --user daemon-reload", output, err)
	}
	if output, err := m.runner().Run(ctx, "systemctl", "--user", "enable", "--now", unit); err != nil {
		return commandFailure("systemctl --user enable --now", output, err)
	}
	return nil
}

func (m SystemdManager) Stop(ctx context.Context, unit string) error {
	if !safeSystemdUnit(unit) {
		return errors.New("safe systemd service unit is required")
	}
	output, err := m.runner().Run(ctx, "systemctl", "--user", "stop", unit)
	if err != nil {
		return commandFailure("systemctl --user stop", output, err)
	}
	return nil
}

func (m SystemdManager) Status(ctx context.Context, unit string, mountPoint string) Status {
	result := Status{}
	if !safeSystemdUnit(unit) {
		result.DaemonError = "safe systemd service unit is required"
	} else {
		output, err := m.runner().Run(ctx, "systemctl", "--user", "show", unit, "--property=ActiveState", "--property=SubState", "--no-pager")
		if err != nil {
			result.DaemonError = commandFailure("systemctl --user show", output, err).Error()
		} else if systemdStateRunning(output) {
			result.DaemonRunning = true
		} else {
			result.DaemonError = "systemd user service is loaded but not running"
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

func (m SystemdManager) WaitHealthy(ctx context.Context, unit string, mountPoint string, timeout time.Duration) (Status, error) {
	return waitHealthy(ctx, timeout, func() Status { return m.Status(ctx, unit, mountPoint) })
}

func (m SystemdManager) runner() Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return ExecRunner{}
}

func quoteSystemdArgument(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("systemd arguments cannot contain NUL or newlines")
	}
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "%", "%%")
	return "\"" + value + "\"", nil
}

func escapeSystemdSettingValue(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("systemd setting values cannot contain NUL or newlines")
	}
	var output strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character == '%':
			output.WriteString("%%")
		case character <= 0x20 || character == '\\' || character == '"':
			_, _ = fmt.Fprintf(&output, "\\x%02x", character)
		default:
			output.WriteByte(character)
		}
	}
	return output.String(), nil
}

func safeSystemdUnit(unit string) bool {
	return strings.HasSuffix(unit, ".service") && safeLabel(strings.TrimSuffix(unit, ".service"))
}

func systemdStateRunning(output []byte) bool {
	active := false
	running := false
	for _, line := range strings.Split(string(output), "\n") {
		switch strings.TrimSpace(line) {
		case "ActiveState=active":
			active = true
		case "SubState=running":
			running = true
		}
	}
	return active && running
}

func commandFailure(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, message)
}
