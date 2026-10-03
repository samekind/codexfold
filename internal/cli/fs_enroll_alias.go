package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/enroll"
)

// Older mounted hosts do not recognize a verbatim drive prefix when deriving
// a path relative to the Codex home. Normalize only the selected idle session,
// after proving both spellings identify the same unchanged file. No file moves.
func normalizeEnrollmentAlias(ctx context.Context, home string, decision *enroll.Decision) error {
	if runtime.GOOS != "windows" || !strings.HasPrefix(decision.RolloutPath, `\\?\`) {
		return nil
	}
	plain := strings.TrimPrefix(decision.RolloutPath, `\\?\`)
	if len(filepath.VolumeName(plain)) != 2 || !filepath.IsAbs(plain) {
		return errors.New("unsupported extended enrollment path")
	}
	if _, err := canonicalRelativeRoute(home, plain); err != nil {
		return err
	}
	original, err := os.Stat(decision.RolloutPath)
	if err != nil {
		return err
	}
	target, err := os.Stat(plain)
	if err != nil {
		return err
	}
	if !original.Mode().IsRegular() || !os.SameFile(original, target) || original.Size() != decision.Fingerprint.Size || original.ModTime().UnixNano() != decision.Fingerprint.ModTimeUnixNano {
		return errors.New("enrollment path alias identity or fingerprint changed")
	}
	digest, err := hashPath(plain)
	if err != nil {
		return err
	}
	if digest.Bytes != decision.Fingerprint.Size {
		return errors.New("enrollment alias changed while hashing")
	}
	if _, err := codex.RouteSession(ctx, codex.RouteOptions{CodexHome: home, SessionID: decision.SessionID, ExpectedPath: decision.RolloutPath, Target: codex.RouteTarget{Path: plain, Bytes: digest.Bytes, SHA256: digest.SHA256}}); err != nil {
		return err
	}
	decision.RolloutPath = plain
	return nil
}
