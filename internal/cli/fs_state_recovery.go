package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

func recoverManagedSessionStateIssues(
	store string,
	sessions []codex.Session,
	issues []vfs.SessionStateIssue,
	deleted map[string]struct{},
) (bool, error) {
	store = filepath.Clean(store)
	if !filepath.IsAbs(store) {
		return false, errors.New("absolute managed store path is required")
	}
	deletionLock, err := storage.AcquireOperationLock(store, "session-deletions")
	if err != nil {
		return false, fmt.Errorf("serialize managed state recovery with explicit deletion: %w", err)
	}
	defer deletionLock.Close()
	currentDeletions, err := vfs.DiscoverSessionDeletions(store)
	if err != nil {
		return false, fmt.Errorf("fix deletion authority before managed state recovery: %w", err)
	}
	if deleted == nil {
		deleted = make(map[string]struct{}, len(currentDeletions))
	}
	for _, deletion := range currentDeletions {
		deleted[deletion.SessionID] = struct{}{}
	}
	current := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		if validSessionID(session.ID) {
			current[session.ID] = struct{}{}
		}
	}
	recovered := false
	seen := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if issue.Kind == vfs.SessionStateIssueStaging {
			continue
		}
		if _, exists := current[issue.SessionID]; !exists {
			continue
		}
		if _, exists := deleted[issue.SessionID]; exists {
			continue
		}
		if _, duplicate := seen[issue.SessionID]; duplicate {
			continue
		}
		seen[issue.SessionID] = struct{}{}
		expected := filepath.Join(store, "fs", "sessions", issue.SessionID, "state.json")
		if filepath.Clean(issue.Path) != expected {
			return recovered, fmt.Errorf("managed session %s issue is not its canonical state path", issue.SessionID)
		}
		state, err := vfs.LoadSessionState(expected)
		if err != nil {
			return recovered, fmt.Errorf("recover managed session state %s: %w", issue.SessionID, err)
		}
		if state.SessionID != issue.SessionID {
			return recovered, fmt.Errorf("recovered managed session state %s belongs to %s", issue.SessionID, state.SessionID)
		}
		recovered = true
	}
	return recovered, nil
}
