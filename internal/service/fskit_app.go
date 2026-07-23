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
