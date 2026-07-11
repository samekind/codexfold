//go:build darwin

package compat

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

func DetectDesktopVersion(ctx context.Context, appPath string) (ClientVersion, error) {
	if appPath == "" {
		return ClientVersion{}, errors.New("Codex application path is required")
	}
	plist := appPath + "/Contents/Info.plist"
	short, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", plist).Output()
	if err != nil {
		return ClientVersion{}, err
	}
	build, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleVersion", "raw", plist).Output()
	if err != nil {
		return ClientVersion{}, err
	}
	return ClientVersion{Platform: "darwin", Kind: "desktop", Version: strings.TrimSpace(string(short)) + "+" + strings.TrimSpace(string(build))}, nil
}
