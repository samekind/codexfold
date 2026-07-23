package compat

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

func DetectCLIVersion(ctx context.Context, binary string) (ClientVersion, error) {
	if binary == "" {
		return ClientVersion{}, errors.New("Codex CLI path is required")
	}
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		return ClientVersion{}, err
	}
	fields := strings.Fields(strings.TrimSpace(string(output)))
	if len(fields) < 2 {
		return ClientVersion{}, errors.New("Codex CLI returned an unrecognized version")
	}
	return ClientVersion{Platform: runtime.GOOS, Kind: "cli", Version: fields[len(fields)-1]}, nil
}
