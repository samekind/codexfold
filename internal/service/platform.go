package service

import (
	"errors"
	"runtime"
)

type Platform string

const (
	PlatformLaunchd Platform = "launchd"
	PlatformSystemd Platform = "systemd-user"
	PlatformWindows Platform = "windows-service"
)

func CurrentPlatform() (Platform, error) {
	switch runtime.GOOS {
	case "darwin":
		return PlatformLaunchd, nil
	case "linux":
		return PlatformSystemd, nil
	case "windows":
		return PlatformWindows, nil
	default:
		return "", errors.New("transparent filesystem services are supported only on macOS, Linux, and Windows")
	}
}

func RenderDefinition(platform Platform, options Options) ([]byte, error) {
	switch platform {
	case PlatformLaunchd:
		return RenderLaunchd(options)
	case PlatformSystemd:
		return RenderSystemd(options)
	case PlatformWindows:
		return RenderWindowsConfig(options)
	default:
		return nil, errors.New("unknown service platform")
	}
}
