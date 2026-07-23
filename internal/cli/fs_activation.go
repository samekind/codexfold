package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/fsctl"
)

func requireFilesystemActivationAllowed(home string) error {
	capability := verifiedCapability()
	if capability != fsctl.FSEnginePreview && capability != fsctl.PlatformCanary {
		return nil
	}

	userHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve real Codex home: %w", err)
	}
	realHomes := []string{filepath.Join(userHome, ".codex")}
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		realHomes = append(realHomes, configured)
	}
	for _, realHome := range realHomes {
		if sameActivationPath(home, realHome) {
			return fmt.Errorf("real Codex home activation is disabled while filesystem capability is %s; only an isolated compatibility canary is allowed", capability)
		}
	}
	return nil
}

func sameActivationPath(left string, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return true
	}
	resolvedLeft, leftErr := filepath.EvalSymlinks(left)
	resolvedRight, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(resolvedLeft) == filepath.Clean(resolvedRight)
}
