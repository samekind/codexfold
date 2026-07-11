package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
)

func (s *Session) recover(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	records, err := readJournal(s.directory)
	if err != nil {
		return err
	}
	latest := make(map[string]JournalRecord)
	lastPosition := make(map[string]int)
	for index, record := range records {
		if record.OperationID == "" {
			return errors.New("session journal record has no operation ID")
		}
		latest[record.OperationID] = record
		lastPosition[record.OperationID] = index
	}
	ordered := make([]JournalRecord, 0, len(latest))
	for _, record := range latest {
		ordered = append(ordered, record)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return lastPosition[ordered[i].OperationID] < lastPosition[ordered[j].OperationID]
	})
	for _, record := range ordered {
		switch record.Phase {
		case "complete", "rolled-back":
			continue
		case "after-file-publish", "state-publishing", "state-published":
			if record.Candidate.SessionID != s.state.SessionID || record.Candidate.Generation == 0 || !pathWithin(s.directory, record.Candidate.DeltaPath) || (record.Candidate.BackingPath != "" && !pathWithin(s.directory, record.Candidate.BackingPath)) {
				return fmt.Errorf("journal operation %s has unsafe candidate state", record.OperationID)
			}
			state, err := loadSessionState(s.statePath)
			if err != nil {
				return err
			}
			if record.Kind == "compact" {
				if state.Generation < record.Candidate.Generation {
					_ = os.Remove(record.Candidate.DeltaPath)
					if err := appendJournal(s.directory, JournalRecord{OperationID: record.OperationID, SessionID: record.SessionID, Kind: record.Kind, Phase: "rolled-back", Candidate: record.Candidate, FinalPath: record.FinalPath}); err != nil {
						return err
					}
					continue
				}
				if state.Generation != record.Candidate.Generation || state.ManifestPath != record.Candidate.ManifestPath {
					return fmt.Errorf("journal operation %s conflicts with current compacted state", record.OperationID)
				}
				s.state = state
			} else {
				if record.FinalPath == "" || record.Candidate.BackingPath == "" {
					return fmt.Errorf("journal operation %s has incomplete published state", record.OperationID)
				}
				verified, err := hashNativePath(record.FinalPath)
				if err != nil || verified.Bytes != record.Native.Bytes || verified.SHA256 != record.Native.SHA256 {
					return fmt.Errorf("journal operation %s published backing cannot be verified: %w", record.OperationID, err)
				}
				if state.Generation < record.Candidate.Generation {
					if err := writeSessionState(s.statePath, record.Candidate); err != nil {
						return err
					}
					s.state = record.Candidate
				}
			}
			if err := appendJournal(s.directory, JournalRecord{OperationID: record.OperationID, SessionID: record.SessionID, Kind: record.Kind, Phase: "complete", Candidate: record.Candidate, FinalPath: record.FinalPath, Native: record.Native}); err != nil {
				return err
			}
		case "prepared", "data-synced":
			if record.TempPath != "" {
				_ = os.Remove(record.TempPath)
			}
			if record.Kind == "compact" && record.FinalPath != "" {
				_ = os.Remove(record.FinalPath)
			}
			if err := appendJournal(s.directory, JournalRecord{OperationID: record.OperationID, SessionID: record.SessionID, Kind: record.Kind, Phase: "rolled-back", Candidate: record.Candidate, TempPath: record.TempPath}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("journal operation %s has unknown phase %q", record.OperationID, record.Phase)
		}
	}
	return nil
}

func (s *Session) Recover(ctx context.Context) error { return s.recover(ctx) }
