package service

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/buildid"
)

func windowsDefinitionPath(definitionPath, flag string) (string, error) {
	if !filepath.IsAbs(definitionPath) {
		return "", errors.New("absolute service definition path is required")
	}
	data, err := os.ReadFile(definitionPath)
	if err != nil {
		return "", err
	}
	config, err := ParseWindowsConfig(data)
	if err != nil {
		return "", err
	}
	value := ""
	for index, argument := range config.Arguments {
		if argument == flag && index+1 < len(config.Arguments) {
			value = config.Arguments[index+1]
		}
		if strings.HasPrefix(argument, flag+"=") {
			value = strings.TrimPrefix(argument, flag+"=")
		}
	}
	if !filepath.IsAbs(value) && !absoluteWindowsServicePath(value) {
		return "", errors.New("Windows definition requires an absolute " + flag + " argument")
	}
	return filepath.Clean(value), nil
}

// The SCM binary owns the resident frontend. A verified, administrator-owned
// image binding selects the separately replaceable storage engine.
func configuredWindowsCoreImage(binary string) (string, error) {
	root := filepath.Join(filepath.Dir(binary), "Core")
	data, err := os.ReadFile(filepath.Join(root, "current.json"))
	if errors.Is(err, os.ErrNotExist) {
		return binary, nil
	}
	if err != nil {
		return "", err
	}
	var image struct {
		Version int
		SHA256  string
	}
	if json.Unmarshal(data, &image) != nil || image.Version != 1 || !buildid.ValidSHA256(image.SHA256) {
		return "", errors.New("configured Windows engine binding is invalid")
	}
	path := filepath.Join(root, image.SHA256+".exe")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("configured Windows engine is not an ordinary file")
	}
	if actual, err := buildid.FileSHA256(path); err != nil || actual != image.SHA256 {
		return "", errors.New("configured Windows engine hash does not match its binding")
	}
	return path, nil
}
