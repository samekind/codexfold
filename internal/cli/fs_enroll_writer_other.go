//go:build !darwin && !linux && !windows

package cli

import (
	"context"
	"errors"

	"github.com/samekind/codexfold/internal/codex"
)

func detectEnrollmentWriters(context.Context, []codex.Session) (map[string]bool, error) {
	return nil, errors.New("native writer probe is unavailable on this platform")
}
