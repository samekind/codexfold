package service

import (
	"errors"
	"path/filepath"
	"strings"
)

const (
	FSKitAppBundleName         = "CodexFoldFSKit.app"
	FSKitHostExecutableName    = "CodexFoldFSKit"
	FSKitHostBundleIdentifier  = "vip.jstar.codexfold.fskitprofileprobe"
	FSKitModuleBundleName      = "CodexFoldFSKitModule.appex"
	FSKitModuleIdentifier      = "vip.jstar.codexfold.fskitprofileprobe.module"
	FSKitAppGroupIdentifier    = "group.vip.jstar.codexfold"
	FSKitResourceDirectoryName = "native-fskit"
)

func DefaultFSKitAppPath(userHome string) string {
	return filepath.Join(filepath.Clean(userHome), "Applications", FSKitAppBundleName)
}

func FSKitHostLauncherPath(appPath string) (string, error) {
	if !filepath.IsAbs(appPath) {
		return "", errors.New("FSKit app path must be absolute")
	}
	appPath = filepath.Clean(appPath)
	if !strings.HasSuffix(filepath.Base(appPath), ".app") {
		return "", errors.New("FSKit app path must identify an app bundle")
	}
	return filepath.Join(appPath, "Contents", "MacOS", FSKitHostExecutableName), nil
}

func FSKitAppPathFromLauncher(launcher string) (string, error) {
	if !filepath.IsAbs(launcher) {
		return "", errors.New("FSKit launcher path must be absolute")
	}
	launcher = filepath.Clean(launcher)
	if filepath.Base(launcher) != FSKitHostExecutableName {
		return "", errors.New("FSKit launcher path must identify the host executable")
	}
	macosDir := filepath.Dir(launcher)
	contentsDir := filepath.Dir(macosDir)
	appPath := filepath.Dir(contentsDir)
	expected, err := FSKitHostLauncherPath(appPath)
	if err != nil || expected != launcher {
		return "", errors.New("FSKit launcher path is not inside a CodexFold app bundle")
	}
	return appPath, nil
}

func FSKitModulePath(appPath string) (string, error) {
	launcher, err := FSKitHostLauncherPath(appPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(filepath.Dir(launcher)), "Extensions", FSKitModuleBundleName), nil
}

func DefaultFSKitResourcePath(userHome string) string {
	return filepath.Join(filepath.Clean(userHome), "Library", "Group Containers", FSKitAppGroupIdentifier, FSKitResourceDirectoryName)
}

func FSKitStatusDirectory(resourcePath string) string {
	cleaned := filepath.Clean(resourcePath)
	for current := cleaned; ; current = filepath.Dir(current) {
		if filepath.Base(current) == FSKitAppGroupIdentifier {
			defaultResource := filepath.Join(current, FSKitResourceDirectoryName)
			if cleaned == defaultResource {
				return filepath.Join(current, "status")
			}
			return filepath.Join(cleaned, "status")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return filepath.Join(filepath.Dir(cleaned), "status")
}

func FSKitStatusPath(resourcePath, component string) string {
	return filepath.Join(FSKitStatusDirectory(resourcePath), component+".json")
}
