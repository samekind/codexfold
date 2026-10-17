package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const WindowsEnrollmentServiceName = "com.codexfold.enroll"

// This administrator-owned binding contains no executable arguments from the
// writable user policy. Every data location is explicit, even under SYSTEM.
type WindowsEnrollmentConfig struct {
	Version     int    `json:"version"`
	ServiceName string `json:"service_name"`
	BinaryPath  string `json:"binary_path"`
	OwnerSID    string `json:"owner_sid"`
	CodexHome   string `json:"codex_home"`
	Store       string `json:"store"`
	Mount       string `json:"mount"`
	NativeRoot  string `json:"native_root"`
	StdoutPath  string `json:"stdout_path"`
	StderrPath  string `json:"stderr_path"`
}

func ParseWindowsEnrollmentConfig(data []byte) (WindowsEnrollmentConfig, error) {
	var config WindowsEnrollmentConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return config, errors.New("unexpected data after enrollment service binding")
	}
	if config.Version != 1 {
		return config, errors.New("unsupported enrollment service binding version")
	}
	if !safeLabel(config.ServiceName) || (config.ServiceName != WindowsEnrollmentServiceName && !strings.HasPrefix(config.ServiceName, WindowsEnrollmentServiceName+".test-")) {
		return config, errors.New("unexpected enrollment service name")
	}
	if config.OwnerSID == "" {
		return config, errors.New("enrollment service owner SID is required")
	}
	for name, path := range map[string]string{"binary": config.BinaryPath, "home": config.CodexHome, "store": config.Store, "mount": config.Mount, "native": config.NativeRoot, "stdout": config.StdoutPath, "stderr": config.StderrPath} {
		if !absoluteWindowsServicePath(path) {
			return config, fmt.Errorf("absolute enrollment service %s path is required", name)
		}
	}
	if !strings.EqualFold(filepath.Clean(config.Store), filepath.Join(filepath.Clean(config.CodexHome), "fold-store")) ||
		!strings.EqualFold(filepath.Clean(config.NativeRoot), filepath.Join(filepath.Clean(config.CodexHome), "fold-native")) {
		return config, errors.New("enrollment service store and native root must belong to its bound home")
	}
	return config, nil
}
