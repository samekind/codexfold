//go:build darwin || linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/jstar0/codexfold/internal/codex"
)

func detectEnrollmentWriters(ctx context.Context, sessions []codex.Session) (map[string]bool, error) {
	if len(sessions) == 0 {
		return map[string]bool{}, nil
	}
	lsof := "/usr/sbin/lsof"
	if _, err := os.Stat(lsof); err != nil {
		resolved, lookErr := exec.LookPath("lsof")
		if lookErr != nil {
			return nil, fmt.Errorf("native writer probe requires lsof: %w", lookErr)
		}
		lsof = resolved
	}
	output, err := exec.CommandContext(ctx, lsof, "-n", "-P", "-F", "pfan").Output()
	if err != nil {
		return nil, fmt.Errorf("run native writer probe: %w", err)
	}
	return parseEnrollmentWriterSnapshot(output, sessions), nil
}
