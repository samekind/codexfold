package service

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jstar0/codexfold/internal/buildid"
	"github.com/jstar0/codexfold/internal/mountid"
)

type BuildStatus struct {
	Healthy               bool   `json:"healthy"`
	RunningBuildSHA256    string `json:"running_build_sha256,omitempty"`
	ConfiguredBinaryPath  string `json:"configured_binary_path,omitempty"`
	ConfiguredBuildSHA256 string `json:"configured_build_sha256,omitempty"`
	Error                 string `json:"error,omitempty"`
}

func InspectBuild(platform Platform, definitionPath string, mountPoint string) BuildStatus {
	status := BuildStatus{}
	binaryPath, err := DefinitionBinary(platform, definitionPath)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.ConfiguredBinaryPath = binaryPath
	status.ConfiguredBuildSHA256, err = buildid.FileSHA256(binaryPath)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	identityBytes, err := os.ReadFile(filepath.Join(mountPoint, mountid.Path))
	if err != nil {
		status.Error = fmt.Sprintf("read running mount build identity: %v", err)
		return status
	}
	identity, err := mountid.Parse(identityBytes)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.RunningBuildSHA256 = identity.BuildSHA256
	if status.RunningBuildSHA256 == "" {
		status.Error = "running mount identity does not include a build SHA-256"
		return status
	}
	if status.RunningBuildSHA256 != status.ConfiguredBuildSHA256 {
		status.Error = "running daemon build does not match the configured binary on disk"
		return status
	}
	status.Healthy = true
	return status
}

func DefinitionBinary(platform Platform, definitionPath string) (string, error) {
	if !filepath.IsAbs(definitionPath) {
		return "", errors.New("absolute service definition path is required")
	}
	definition, err := os.ReadFile(filepath.Clean(definitionPath))
	if err != nil {
		return "", err
	}
	var binary string
	switch platform {
	case PlatformLaunchd:
		binary, err = launchdDefinitionBinary(definition)
	case PlatformSystemd:
		binary, err = systemdDefinitionBinary(definition)
	case PlatformWindows:
		var config WindowsConfig
		config, err = ParseWindowsConfig(definition)
		binary = config.BinaryPath
	default:
		err = errors.New("unknown service platform")
	}
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(binary) && !absoluteWindowsServicePath(binary) {
		return "", errors.New("configured service binary path is not absolute")
	}
	return filepath.Clean(binary), nil
}

func DefinitionLauncher(platform Platform, definitionPath string) (string, error) {
	if !filepath.IsAbs(definitionPath) {
		return "", errors.New("absolute service definition path is required")
	}
	if platform != PlatformLaunchd {
		return "", nil
	}
	definition, err := os.ReadFile(filepath.Clean(definitionPath))
	if err != nil {
		return "", err
	}
	arguments, err := launchdDefinitionArguments(definition)
	if err != nil {
		return "", err
	}
	if len(arguments) < 3 || arguments[1] != "--run-helper" {
		return "", nil
	}
	if !filepath.IsAbs(arguments[0]) {
		return "", errors.New("configured service launcher path is not absolute")
	}
	return filepath.Clean(arguments[0]), nil
}

func DefinitionFrontend(platform Platform, definitionPath string) (string, error) {
	if !filepath.IsAbs(definitionPath) {
		return "", errors.New("absolute service definition path is required")
	}
	definition, err := os.ReadFile(filepath.Clean(definitionPath))
	if err != nil {
		return "", err
	}
	if platform != PlatformLaunchd {
		return "fuse", nil
	}
	arguments, err := launchdDefinitionArguments(definition)
	if err != nil {
		return "", err
	}
	for index := 0; index < len(arguments); index++ {
		if arguments[index] != "--frontend" {
			continue
		}
		if index+1 >= len(arguments) {
			return "", errors.New("launchd definition has an incomplete --frontend argument")
		}
		if arguments[index+1] != "fuse" && arguments[index+1] != "native-fskit" {
			return "", fmt.Errorf("launchd definition has unsupported frontend %q", arguments[index+1])
		}
		return arguments[index+1], nil
	}
	return "fuse", nil
}

func DefinitionFSKitResource(platform Platform, definitionPath string) (string, error) {
	frontend, err := DefinitionFrontend(platform, definitionPath)
	if err != nil {
		return "", err
	}
	if frontend != "native-fskit" {
		return "", nil
	}
	definition, err := os.ReadFile(filepath.Clean(definitionPath))
	if err != nil {
		return "", err
	}
	arguments, err := launchdDefinitionArguments(definition)
	if err != nil {
		return "", err
	}
	for index := 0; index < len(arguments); index++ {
		if arguments[index] != "--fskit-resource" {
			continue
		}
		if index+1 >= len(arguments) || !filepath.IsAbs(arguments[index+1]) {
			return "", errors.New("launchd definition has an invalid --fskit-resource argument")
		}
		return filepath.Clean(arguments[index+1]), nil
	}
	return "", errors.New("native-fskit launchd definition has no --fskit-resource argument")
}

func DefinitionStore(platform Platform, definitionPath string) (string, error) {
	if !filepath.IsAbs(definitionPath) {
		return "", errors.New("absolute service definition path is required")
	}
	if platform != PlatformLaunchd {
		return "", errors.New("service store inspection is currently available only for launchd definitions")
	}
	definition, err := os.ReadFile(filepath.Clean(definitionPath))
	if err != nil {
		return "", err
	}
	arguments, err := launchdDefinitionArguments(definition)
	if err != nil {
		return "", err
	}
	for index := 0; index < len(arguments); index++ {
		if arguments[index] != "--store" {
			continue
		}
		if index+1 >= len(arguments) || !filepath.IsAbs(arguments[index+1]) {
			return "", errors.New("launchd definition has an invalid --store argument")
		}
		return filepath.Clean(arguments[index+1]), nil
	}
	return "", errors.New("launchd definition has no --store argument")
}

func DefinitionLabel(platform Platform, definitionPath string) (string, error) {
	if !filepath.IsAbs(definitionPath) {
		return "", errors.New("absolute service definition path is required")
	}
	definition, err := os.ReadFile(filepath.Clean(definitionPath))
	if err != nil {
		return "", err
	}
	var label string
	switch platform {
	case PlatformLaunchd:
		label, err = launchdDefinitionLabel(definition)
	case PlatformSystemd:
		label = strings.TrimSuffix(filepath.Base(definitionPath), ".service")
	case PlatformWindows:
		var config WindowsConfig
		config, err = ParseWindowsConfig(definition)
		label = config.ServiceName
	default:
		err = errors.New("unknown service platform")
	}
	if err != nil {
		return "", err
	}
	if !safeLabel(label) {
		return "", errors.New("configured service label is invalid")
	}
	return label, nil
}

func launchdDefinitionBinary(definition []byte) (string, error) {
	arguments, err := launchdDefinitionArguments(definition)
	if err != nil {
		return "", err
	}
	if len(arguments) == 0 {
		return "", errors.New("launchd definition has no ProgramArguments binary")
	}
	if len(arguments) >= 3 && arguments[1] == "--run-helper" {
		return arguments[2], nil
	}
	return arguments[0], nil
}

func launchdDefinitionArguments(definition []byte) ([]string, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(definition)))
	wantArguments := false
	inArguments := false
	arguments := make([]string, 0, 16)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "key":
				var key string
				if err := decoder.DecodeElement(&key, &element); err != nil {
					return nil, err
				}
				wantArguments = key == "ProgramArguments"
			case "array":
				if wantArguments {
					inArguments = true
					wantArguments = false
				}
			case "string":
				if inArguments {
					var argument string
					if err := decoder.DecodeElement(&argument, &element); err != nil {
						return nil, err
					}
					arguments = append(arguments, argument)
				}
			}
		case xml.EndElement:
			if element.Name.Local == "array" && inArguments {
				return arguments, nil
			}
		}
	}
	return nil, errors.New("launchd definition has no ProgramArguments array")
}

func launchdDefinitionLabel(definition []byte) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(definition)))
	wantLabel := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			var key string
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return "", err
			}
			wantLabel = key == "Label"
		case "string":
			if wantLabel {
				var label string
				if err := decoder.DecodeElement(&label, &start); err != nil {
					return "", err
				}
				return label, nil
			}
		}
	}
	return "", errors.New("launchd definition has no Label")
}

func systemdDefinitionBinary(definition []byte) (string, error) {
	for _, line := range strings.Split(string(definition), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "ExecStart=:"))
		if len(value) < 2 || value[0] != '"' {
			return "", errors.New("systemd ExecStart binary is not quoted")
		}
		var binary strings.Builder
		for index := 1; index < len(value); index++ {
			switch value[index] {
			case '"':
				return strings.ReplaceAll(binary.String(), "%%", "%"), nil
			case '\\':
				index++
				if index >= len(value) {
					return "", errors.New("systemd ExecStart binary has an incomplete escape")
				}
				binary.WriteByte(value[index])
			default:
				binary.WriteByte(value[index])
			}
		}
		return "", errors.New("systemd ExecStart binary is missing its closing quote")
	}
	return "", errors.New("systemd definition has no ExecStart binary")
}
