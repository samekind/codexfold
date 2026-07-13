package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jstar0/codexfold/internal/compat"
	"github.com/jstar0/codexfold/internal/fsctl"
)

type Options struct {
	Label              string
	BinaryPath         string
	CodexHome          string
	StoreDir           string
	MountPoint         string
	StdoutPath         string
	StderrPath         string
	CanonicalNamespace bool
	NativeRoot         string
	OperationTrace     string
}

type InstallResult struct {
	Path   string `json:"path"`
	DryRun bool   `json:"dry_run"`
	Bytes  int    `json:"bytes"`
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Manager struct {
	UID        int
	Runner     Runner
	MountProbe func(string) error
}

type Status struct {
	DaemonRunning bool   `json:"daemon_running"`
	MountHealthy  bool   `json:"mount_healthy"`
	DaemonError   string `json:"daemon_error,omitempty"`
	MountError    string `json:"mount_error,omitempty"`
}

type UpdateInput struct {
	Capability          fsctl.Capability
	DoctorHealthy       bool
	Compatibility       compat.Evaluation
	NativeFallbackReady bool
	Automatic           bool
	ExplicitPromotion   bool
}

type UpdateDecision struct {
	Allowed                bool   `json:"allowed"`
	Quarantine             bool   `json:"quarantine"`
	RequiresNativeFallback bool   `json:"requires_native_fallback"`
	Reason                 string `json:"reason,omitempty"`
}

func RenderLaunchd(options Options) ([]byte, error) {
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	arguments := []string{
		options.BinaryPath, "fs", "serve", "--apply", "--foreground=true",
		"--codex-home", options.CodexHome, "--store", options.StoreDir, "--mount", options.MountPoint,
	}
	if options.CanonicalNamespace {
		arguments = append(arguments, "--canonical-namespace", "--native-root", options.NativeRoot)
	}
	if options.OperationTrace != "" {
		arguments = append(arguments, "--operation-trace", options.OperationTrace)
	}
	var output bytes.Buffer
	output.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	output.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	output.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writePlistString(&output, "Label", options.Label)
	output.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, argument := range arguments {
		output.WriteString("    <string>")
		_ = xml.EscapeText(&output, []byte(argument))
		output.WriteString("</string>\n")
	}
	output.WriteString("  </array>\n")
	writePlistString(&output, "StandardOutPath", options.StdoutPath)
	writePlistString(&output, "StandardErrorPath", options.StderrPath)
	output.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	output.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	output.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")
	output.WriteString("</dict>\n</plist>\n")
	return output.Bytes(), nil
}

func WriteDefinition(path string, definition []byte, apply bool) (InstallResult, error) {
	if !filepath.IsAbs(path) || len(definition) == 0 {
		return InstallResult{}, errors.New("absolute definition path and non-empty definition are required")
	}
	result := InstallResult{Path: filepath.Clean(path), DryRun: !apply, Bytes: len(definition)}
	if !apply {
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return InstallResult{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".launchd-*.tmp")
	if err != nil {
		return InstallResult{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return InstallResult{}, err
	}
	if _, err := temporary.Write(definition); err != nil {
		_ = temporary.Close()
		return InstallResult{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return InstallResult{}, err
	}
	if err := temporary.Close(); err != nil {
		return InstallResult{}, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return InstallResult{}, err
	}
	return result, nil
}

func (m Manager) Bootstrap(ctx context.Context, plistPath string) error {
	if !filepath.IsAbs(plistPath) {
		return errors.New("absolute launchd plist path is required")
	}
	_, err := m.runner().Run(ctx, "launchctl", "bootstrap", m.domain(), plistPath)
	return err
}

func (m Manager) Bootout(ctx context.Context, plistPath string) error {
	if !filepath.IsAbs(plistPath) {
		return errors.New("absolute launchd plist path is required")
	}
	_, err := m.runner().Run(ctx, "launchctl", "bootout", m.domain(), plistPath)
	return err
}

func (m Manager) Kickstart(ctx context.Context, label string) error {
	if !safeLabel(label) {
		return errors.New("safe launchd label is required")
	}
	_, err := m.runner().Run(ctx, "launchctl", "kickstart", m.domain()+"/"+label)
	return err
}

func (m Manager) Status(ctx context.Context, label string, mountPoint string) Status {
	result := Status{}
	output, err := m.runner().Run(ctx, "launchctl", "print", m.domain()+"/"+label)
	if err != nil {
		result.DaemonError = err.Error()
	} else if strings.Contains(string(output), "state = running") {
		result.DaemonRunning = true
	} else {
		result.DaemonError = "launchd job is loaded but not running"
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

func (m Manager) WaitHealthy(ctx context.Context, label string, mountPoint string, timeout time.Duration) (Status, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last Status
	for {
		last = m.Status(ctx, label, mountPoint)
		if last.DaemonRunning && last.MountHealthy {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-deadline.C:
			return last, fmt.Errorf("filesystem service did not become healthy: daemon=%t mount=%t daemon_error=%q mount_error=%q", last.DaemonRunning, last.MountHealthy, last.DaemonError, last.MountError)
		case <-ticker.C:
		}
	}
}

func ProbeMount(path string) error { return defaultMountProbe(path) }

func EvaluateUpdate(input UpdateInput) UpdateDecision {
	if !input.DoctorHealthy {
		return UpdateDecision{Reason: "filesystem doctor is not healthy"}
	}
	if input.Compatibility.Quarantine || !input.Compatibility.Approved {
		return UpdateDecision{Quarantine: true, RequiresNativeFallback: !input.NativeFallbackReady, Reason: "installed client version is not approved"}
	}
	if input.Automatic && (input.Capability == fsctl.FSEnginePreview || input.Capability == fsctl.PlatformCanary) {
		return UpdateDecision{Reason: "automatic updates are disabled before platform production readiness"}
	}
	if (input.Capability == fsctl.FSEnginePreview || input.Capability == fsctl.PlatformCanary) && !input.ExplicitPromotion {
		return UpdateDecision{Reason: "preview and canary updates require explicit promotion"}
	}
	return UpdateDecision{Allowed: true}
}

func (m Manager) runner() Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return ExecRunner{}
}

func (m Manager) domain() string {
	uid := m.UID
	if uid <= 0 {
		uid = os.Getuid()
	}
	return fmt.Sprintf("gui/%d", uid)
}

func validateOptions(options Options) error {
	if !safeLabel(options.Label) {
		return errors.New("safe launchd label is required")
	}
	for name, path := range map[string]string{
		"binary": options.BinaryPath, "Codex home": options.CodexHome, "store": options.StoreDir,
		"mount": options.MountPoint, "stdout": options.StdoutPath, "stderr": options.StderrPath,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s path must be absolute", name)
		}
	}
	if options.CanonicalNamespace && !filepath.IsAbs(options.NativeRoot) {
		return errors.New("canonical namespace requires an absolute native root")
	}
	if options.OperationTrace != "" && !filepath.IsAbs(options.OperationTrace) {
		return errors.New("operation trace path must be absolute")
	}
	return nil
}

func safeLabel(label string) bool {
	if label == "" || strings.HasPrefix(label, ".") || strings.HasSuffix(label, ".") {
		return false
	}
	for _, character := range label {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func writePlistString(output *bytes.Buffer, key string, value string) {
	output.WriteString("  <key>")
	_ = xml.EscapeText(output, []byte(key))
	output.WriteString("</key>\n  <string>")
	_ = xml.EscapeText(output, []byte(value))
	output.WriteString("</string>\n")
}
