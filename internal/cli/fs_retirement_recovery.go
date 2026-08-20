package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

const (
	canonicalRetirementRecoveryLock = "canonical-retirement-recovery"
	maximumRetirementJSONBytes      = 1 << 20

	retirementPhaseStateRetired         = "state-retired"
	retirementPhaseNativeRetired        = "native-retired"
	retirementPhaseRequestPublished     = "request-published"
	retirementPhaseNativeRestored       = "native-restored"
	retirementPhaseStateRestored        = "state-restored"
	retirementPhaseStateRepublished     = "state-republished"
	retirementPhaseBeforeOwnerHandoff   = "before-owner-handoff"
	retirementPhaseBeforeCommittedMove  = "before-committed-mutation"
	retirementPhaseOwnerDetached        = "owner-detached"
	retirementPhaseBeforeRequestCleared = "before-request-cleared"
	retirementPhaseRequestCleared       = "request-cleared"
	retirementPhaseAcknowledgementGone  = retirementPhaseBeforeRequestCleared
)

type canonicalRetirementRecoveryResult struct {
	Completed           int
	Restored            int
	Deferred            int
	CompletedSessionIDs []string
	RestoredSessionIDs  []string
	DeferredSessionIDs  []string
}

type canonicalRetirementRecoveryHook func(sessionID string, phase string) error

type canonicalRetirementOwnerHandoff func(sessionID string, expectedRoute string, commit func() error) error

type pendingCanonicalRetirement struct {
	SessionID string
	Directory string
	Active    bool
}

type retirementFileIdentity struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type retirementStateCheckpoint struct {
	Version                  int                     `json:"version"`
	SessionID                string                  `json:"session_id"`
	Sequence                 uint64                  `json:"sequence"`
	PreviousCheckpointSHA256 string                  `json:"previous_checkpoint_sha256,omitempty"`
	State                    vfs.SessionState        `json:"state"`
	StateSHA256              string                  `json:"state_sha256"`
	Manifest                 retirementFileIdentity  `json:"manifest"`
	Delta                    retirementFileIdentity  `json:"delta"`
	Backing                  *retirementFileIdentity `json:"backing,omitempty"`
	NativeSnapshot           *retirementFileIdentity `json:"native_snapshot,omitempty"`
}

type retirementStateCatalog struct {
	Version          int    `json:"version"`
	SessionID        string `json:"session_id"`
	Generation       uint64 `json:"generation"`
	Sequence         uint64 `json:"sequence"`
	StateSHA256      string `json:"state_sha256"`
	Checkpoint       string `json:"checkpoint"`
	CheckpointSHA256 string `json:"checkpoint_sha256"`
}

type retirementStateProof struct {
	State                 vfs.SessionState
	Checkpoint            retirementStateCheckpoint
	RequestBindingMatches bool
}

type retirementStateBinding struct {
	StateSHA256        string
	CheckpointSequence uint64
	CheckpointSHA256   string
}

type retirementSnapshotProof struct {
	OriginalPath string
	RetiredPath  string
	AtOriginal   bool
	AtRetired    bool
}

type canonicalRetirementPlan struct {
	Pending       pendingCanonicalRetirement
	Request       retirementControl
	StateProof    retirementStateProof
	Snapshot      retirementSnapshotProof
	TargetPath    string
	CurrentRoute  string
	TargetMatches bool
	RouteMatches  bool
	Acknowledged  bool
	Rejected      bool
	Rejection     string
	Restore       bool
	Guard         *vfs.WriterLeaseGuard
}

type retirementAcknowledgementStatus struct {
	Acknowledged bool
	Rejected     bool
	Error        string
}

var errRetirementNamespaceChanged = errors.New("canonical retirement namespace changed during discovery")

// recoverCanonicalRetirements closes canonical rollback transactions whose
// owner disappeared after publishing retire.request.json. A request that has
// already disappeared is complete and is never inferred from other metadata.
func recoverCanonicalRetirements(ctx context.Context, home string, store string, mount string, nativeRoot string) (canonicalRetirementRecoveryResult, error) {
	return recoverCanonicalRetirementsWithOptions(ctx, home, store, mount, nativeRoot, nil, nil)
}

func recoverCanonicalRetirementsWithHook(
	ctx context.Context,
	home string,
	store string,
	mount string,
	nativeRoot string,
	hook canonicalRetirementRecoveryHook,
) (canonicalRetirementRecoveryResult, error) {
	return recoverCanonicalRetirementsWithOptions(ctx, home, store, mount, nativeRoot, hook, nil)
}

// recoverCanonicalRetirementsWithOwnerHandoff keeps retire.request.json
// durable until the caller has idempotently detached the in-memory owner.
// Production daemon recovery must use this entry point; a crash before or
// after handoff simply replays the callback while the request still exists.
func recoverCanonicalRetirementsWithOwnerHandoff(
	ctx context.Context,
	home string,
	store string,
	mount string,
	nativeRoot string,
	handoff canonicalRetirementOwnerHandoff,
) (canonicalRetirementRecoveryResult, error) {
	if handoff == nil {
		return canonicalRetirementRecoveryResult{}, errors.New("canonical retirement owner handoff is required")
	}
	return recoverCanonicalRetirementsWithOptions(ctx, home, store, mount, nativeRoot, nil, handoff)
}

func recoverCanonicalRetirementsWithOptions(
	ctx context.Context,
	home string,
	store string,
	mount string,
	nativeRoot string,
	hook canonicalRetirementRecoveryHook,
	handoff canonicalRetirementOwnerHandoff,
) (canonicalRetirementRecoveryResult, error) {
	var result canonicalRetirementRecoveryResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	home = filepath.Clean(home)
	store = filepath.Clean(store)
	mount = filepath.Clean(mount)
	nativeRoot = filepath.Clean(nativeRoot)
	if !filepath.IsAbs(home) || !filepath.IsAbs(store) || !filepath.IsAbs(mount) || !filepath.IsAbs(nativeRoot) {
		return result, errors.New("canonical retirement recovery requires absolute roots")
	}

	pending, err := discoverPendingCanonicalRetirements(store)
	if err != nil || len(pending) == 0 {
		return result, err
	}

	requiresCodexSnapshot, err := canonicalRetirementsRequireCodexSnapshot(pending)
	if err != nil {
		return result, err
	}
	// PREPARED compensation needs a complete Codex snapshot. COMMITTED
	// transactions trust their durable ACK and can finish while SQLite is
	// temporarily unavailable; they never use metadata loss as compensation.
	if requiresCodexSnapshot {
		if _, err := codex.LoadSessions(home); err != nil {
			return result, fmt.Errorf("load Codex metadata before retirement recovery: %w", err)
		}
	}
	operation, err := storage.AcquireOperationLock(store, canonicalRetirementRecoveryLock)
	if errors.Is(err, storage.ErrOperationLockHeld) {
		deferCanonicalRetirements(&result, pending)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer operation.Close()
	deletionLock, err := storage.AcquireOperationLock(store, "session-deletions")
	if errors.Is(err, storage.ErrOperationLockHeld) {
		deferCanonicalRetirements(&result, pending)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer deletionLock.Close()

	if err := ctx.Err(); err != nil {
		return result, err
	}
	pending, err = discoverPendingCanonicalRetirements(store)
	if err != nil || len(pending) == 0 {
		return result, err
	}
	requiresCodexSnapshot, err = canonicalRetirementsRequireCodexSnapshot(pending)
	if err != nil {
		return result, err
	}
	var sessions []codex.Session
	if requiresCodexSnapshot {
		sessions, err = codex.LoadSessions(home)
		if err != nil {
			return result, fmt.Errorf("reload Codex metadata under retirement recovery lock: %w", err)
		}
	}

	plans := make([]canonicalRetirementPlan, 0, len(pending))
	closePlans := func() error {
		var closeErr error
		for index := range plans {
			closeErr = errors.Join(closeErr, plans[index].Guard.Close())
			plans[index].Guard = nil
		}
		return closeErr
	}
	defer closePlans()

	for _, transaction := range pending {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		guard, acquired, err := vfs.TryAcquireWriterLeaseGuardAtPath(filepath.Join(transaction.Directory, "writer.lease"))
		if err != nil {
			return result, fmt.Errorf("reserve retirement writer lease for %s: %w", transaction.SessionID, err)
		}
		if !acquired {
			result.Deferred++
			result.DeferredSessionIDs = append(result.DeferredSessionIDs, transaction.SessionID)
			continue
		}
		if _, err := vfs.LoadSessionDeletion(store, transaction.SessionID); err == nil {
			_ = guard.Close()
			return result, fmt.Errorf("explicit deletion tombstone supersedes pending canonical retirement %s", transaction.SessionID)
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = guard.Close()
			return result, fmt.Errorf("inspect deletion authority before retirement recovery %s: %w", transaction.SessionID, err)
		}
		if err := cleanupStaleRetirementControlTemporaries(transaction.Directory); err != nil {
			_ = guard.Close()
			return result, fmt.Errorf("clean stale retirement control temporaries for %s: %w", transaction.SessionID, err)
		}
		if transaction.Active {
			if _, err := vfs.RecoverSessionJournalWithWriterLease(ctx, filepath.Join(transaction.Directory, "state.json")); err != nil {
				_ = guard.Close()
				return result, fmt.Errorf("recover active retirement session journal for %s: %w", transaction.SessionID, err)
			}
		}
		plan, err := prepareCanonicalRetirementPlan(home, store, mount, nativeRoot, sessions, transaction)
		if err != nil {
			_ = guard.Close()
			return result, fmt.Errorf("validate pending canonical retirement %s: %w", transaction.SessionID, err)
		}
		activeReaders, err := retirementReadersActive(transaction.Directory)
		if err != nil {
			_ = guard.Close()
			return result, fmt.Errorf("inspect retirement reader leases for %s: %w", transaction.SessionID, err)
		}
		if activeReaders {
			_ = guard.Close()
			result.Deferred++
			result.DeferredSessionIDs = append(result.DeferredSessionIDs, transaction.SessionID)
			continue
		}
		plan.Guard = guard
		plans = append(plans, plan)
	}

	for index := range plans {
		plan := &plans[index]
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var freshSessions []codex.Session
		if !plan.Acknowledged {
			freshSessions, err = codex.LoadSessions(home)
			if err != nil {
				return result, fmt.Errorf("refresh Codex metadata before retirement mutation %s: %w", plan.Pending.SessionID, err)
			}
		}
		freshPlan, err := prepareCanonicalRetirementPlan(home, store, mount, nativeRoot, freshSessions, plan.Pending)
		if err != nil {
			return result, fmt.Errorf("refresh pending canonical retirement %s: %w", plan.Pending.SessionID, err)
		}
		freshPlan.Guard = plan.Guard
		plans[index] = freshPlan
		plan = &plans[index]
		restored, err := completeCanonicalRetirement(ctx, home, store, mount, nativeRoot, plan, hook, handoff)
		if err != nil {
			return result, fmt.Errorf("complete interrupted canonical retirement %s: %w", plan.Pending.SessionID, err)
		}
		if restored {
			result.Restored++
			result.RestoredSessionIDs = append(result.RestoredSessionIDs, plan.Pending.SessionID)
			continue
		}
		result.Completed++
		result.CompletedSessionIDs = append(result.CompletedSessionIDs, plan.Pending.SessionID)
	}
	return result, closePlans()
}

func canonicalRetirementsRequireCodexSnapshot(pending []pendingCanonicalRetirement) (bool, error) {
	for _, transaction := range pending {
		request, err := loadRetirementRequestAt(transaction.Directory)
		if err != nil {
			return false, err
		}
		acknowledgement, err := validateRetirementAcknowledgement(transaction.Directory, request)
		if err != nil {
			return false, err
		}
		if !acknowledgement.Acknowledged {
			return true, nil
		}
	}
	return false, nil
}

func deferCanonicalRetirements(result *canonicalRetirementRecoveryResult, pending []pendingCanonicalRetirement) {
	result.Deferred += len(pending)
	for _, transaction := range pending {
		result.DeferredSessionIDs = append(result.DeferredSessionIDs, transaction.SessionID)
	}
}

func discoverPendingCanonicalRetirements(store string) ([]pendingCanonicalRetirement, error) {
	for range 16 {
		pending, err := discoverPendingCanonicalRetirementsOnce(store)
		if !errors.Is(err, errRetirementNamespaceChanged) {
			return pending, err
		}
		runtime.Gosched()
	}
	return nil, errors.New("canonical retirement namespace kept changing during discovery")
}

func discoverPendingCanonicalRetirementsOnce(store string) ([]pendingCanonicalRetirement, error) {
	store = filepath.Clean(store)
	var pending []pendingCanonicalRetirement
	for _, root := range []struct {
		path   string
		active bool
	}{
		{filepath.Join(store, "fs", "sessions"), true},
		{filepath.Join(store, "fs", "retired"), false},
	} {
		entries, err := os.ReadDir(root.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rootInfo, err := os.Lstat(root.path)
		if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("retirement recovery root is not a real directory: %s", root.path)
		}
		for _, entry := range entries {
			directory := filepath.Join(root.path, entry.Name())
			info, err := os.Lstat(directory)
			if errors.Is(err, os.ErrNotExist) {
				return nil, errRetirementNamespaceChanged
			}
			if err != nil {
				return nil, err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			requestInfo, err := os.Lstat(filepath.Join(directory, retirementRequestFilename))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !requestInfo.Mode().IsRegular() {
				return nil, fmt.Errorf("retirement request is not a regular file: %s", directory)
			}
			stateData, _, err := readStableRetirementFile(filepath.Join(directory, "state.json"))
			if errors.Is(err, os.ErrNotExist) {
				return nil, errRetirementNamespaceChanged
			}
			if err != nil {
				return nil, fmt.Errorf("read pending retirement state %s: %w", directory, err)
			}
			var state struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal(stateData, &state); err != nil || !validSessionID(state.SessionID) {
				return nil, fmt.Errorf("pending retirement contains an invalid session state: %s", directory)
			}
			if root.active {
				if entry.Name() != state.SessionID {
					return nil, errors.New("active retirement directory does not match its session ID")
				}
			} else if !validRetiredStateDirectoryName(entry.Name(), state.SessionID) {
				return nil, errors.New("retired transaction directory does not match its session ID")
			}
			pending = append(pending, pendingCanonicalRetirement{SessionID: state.SessionID, Directory: directory, Active: root.active})
		}
	}

	bySession := make(map[string][]pendingCanonicalRetirement)
	for _, transaction := range pending {
		bySession[transaction.SessionID] = append(bySession[transaction.SessionID], transaction)
	}
	for sessionID, transactions := range bySession {
		if len(transactions) != 1 {
			for _, transaction := range transactions {
				if _, err := os.Lstat(transaction.Directory); errors.Is(err, os.ErrNotExist) {
					return nil, errRetirementNamespaceChanged
				} else if err != nil {
					return nil, err
				}
			}
			return nil, fmt.Errorf("multiple unfinished retirement transactions exist for session %s", sessionID)
		}
		if transactions[0].Active {
			continue
		}
		activePath := filepath.Join(store, "fs", "sessions", sessionID)
		if _, err := os.Lstat(activePath); err == nil {
			return nil, fmt.Errorf("active and retired state both exist for session %s", sessionID)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].SessionID == pending[j].SessionID {
			return pending[i].Directory < pending[j].Directory
		}
		return pending[i].SessionID < pending[j].SessionID
	})
	return pending, nil
}

func validRetiredStateDirectoryName(name string, sessionID string) bool {
	prefix := sessionID + "-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	nanoseconds, err := strconv.ParseInt(strings.TrimPrefix(name, prefix), 10, 64)
	return err == nil && nanoseconds > 0
}

func prepareCanonicalRetirementPlan(
	home string,
	store string,
	mount string,
	nativeRoot string,
	sessions []codex.Session,
	pending pendingCanonicalRetirement,
) (canonicalRetirementPlan, error) {
	request, err := loadRetirementRequestAt(pending.Directory)
	if err != nil {
		return canonicalRetirementPlan{}, err
	}
	acknowledgement, err := validateRetirementAcknowledgement(pending.Directory, request)
	if err != nil {
		return canonicalRetirementPlan{}, err
	}
	stateProof, err := loadRetirementStateProof(store, pending, request)
	if err != nil {
		return canonicalRetirementPlan{}, err
	}
	if acknowledgement.Acknowledged && !stateProof.RequestBindingMatches {
		return canonicalRetirementPlan{}, errors.New("acknowledged retirement managed checkpoint changed after cutover")
	}

	var session *codex.Session
	for index := range sessions {
		if sessions[index].ID != pending.SessionID {
			continue
		}
		if session != nil {
			return canonicalRetirementPlan{}, errors.New("Codex metadata contains duplicate session IDs")
		}
		session = &sessions[index]
	}
	requestRolloutPath := filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(request.Route, "/")))
	normalizedRequestRoute, err := canonicalNamespaceRoute(home, mount, requestRolloutPath)
	if err != nil || normalizedRequestRoute != request.Route {
		return canonicalRetirementPlan{}, errors.New("retirement request contains an unsafe canonical route")
	}
	routeMatches := false
	currentRoute := ""
	if session != nil {
		route, routeErr := canonicalNamespaceRoute(home, mount, session.RolloutPath)
		if routeErr == nil {
			currentRoute = route
			routeMatches = route == request.Route
		}
	}
	targetPath, err := canonicalNativeRoute(home, nativeRoot, requestRolloutPath)
	if err != nil {
		return canonicalRetirementPlan{}, err
	}
	var targetMatches bool
	if acknowledgement.Acknowledged {
		// The positive acknowledgement is the durable linearization point: the
		// exact checkpoint and native target were verified while the namespace
		// owner was atomically removed. Later native appends, renames, rewrites,
		// or explicit deletion belong to native authority. Preserve managed
		// evidence in retired state, but never turn those later changes into a
		// managed rollback or a stalled completion.
		targetMatches = true
	} else {
		targetMatches, err = canonicalRetirementTargetStillMatches(nativeRoot, targetPath, request)
		if err != nil {
			return canonicalRetirementPlan{}, err
		}
	}

	if stateProof.State.Generation == request.Generation+1 {
		if !pending.Active {
			return canonicalRetirementPlan{}, errors.New("retired state advanced beyond the retirement request")
		}
	} else if stateProof.State.Generation != request.Generation {
		return canonicalRetirementPlan{}, errors.New("session state generation does not match the retirement request")
	}

	snapshot, err := validateRetirementSnapshot(store, nativeRoot, pending, stateProof.State, targetPath, request, targetMatches, acknowledgement.Acknowledged)
	if err != nil {
		return canonicalRetirementPlan{}, err
	}
	restore := !acknowledgement.Acknowledged && (acknowledgement.Rejected || !stateProof.RequestBindingMatches || stateProof.State.Generation == request.Generation+1 || !routeMatches || !targetMatches)
	return canonicalRetirementPlan{
		Pending: pending, Request: request, StateProof: stateProof, Snapshot: snapshot,
		TargetPath: targetPath, CurrentRoute: currentRoute, TargetMatches: targetMatches, RouteMatches: routeMatches,
		Acknowledged: acknowledgement.Acknowledged, Rejected: acknowledgement.Rejected, Rejection: acknowledgement.Error, Restore: restore,
	}, nil
}

func loadRetirementRequestAt(directory string) (retirementControl, error) {
	data, _, err := readStableRetirementFile(filepath.Join(directory, retirementRequestFilename))
	if err != nil {
		return retirementControl{}, err
	}
	var request retirementControl
	if err := decodeStrictRetirementJSON(data, &request); err != nil {
		return retirementControl{}, fmt.Errorf("decode retirement request: %w", err)
	}
	decodedToken, tokenErr := hex.DecodeString(request.Token)
	if tokenErr != nil || len(decodedToken) != 16 || request.Generation == 0 || !validRetirementSHA256(request.StateSHA256) || request.CheckpointSequence == 0 || !validRetirementSHA256(request.CheckpointSHA256) || request.Route == "" || request.Bytes < 0 || !validRetirementSHA256(request.SHA256) || request.Error != "" {
		return retirementControl{}, errors.New("invalid retirement request")
	}
	return request, nil
}

func validateRetirementAcknowledgement(directory string, request retirementControl) (retirementAcknowledgementStatus, error) {
	path := filepath.Join(directory, retirementAcknowledgementFilename)
	data, _, err := readStableRetirementFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return retirementAcknowledgementStatus{}, nil
	}
	if err != nil {
		return retirementAcknowledgementStatus{}, err
	}
	var acknowledgement retirementControl
	if err := decodeStrictRetirementJSON(data, &acknowledgement); err != nil {
		return retirementAcknowledgementStatus{}, fmt.Errorf("decode retirement acknowledgement: %w", err)
	}
	if acknowledgement.Token != request.Token || acknowledgement.Generation != request.Generation || acknowledgement.StateSHA256 != request.StateSHA256 || acknowledgement.CheckpointSequence != request.CheckpointSequence || acknowledgement.CheckpointSHA256 != request.CheckpointSHA256 || acknowledgement.Route != request.Route || acknowledgement.Bytes != request.Bytes || acknowledgement.SHA256 != request.SHA256 {
		return retirementAcknowledgementStatus{}, errors.New("retirement acknowledgement does not match its request")
	}
	return retirementAcknowledgementStatus{
		Acknowledged: acknowledgement.Error == "",
		Rejected:     acknowledgement.Error != "",
		Error:        acknowledgement.Error,
	}, nil
}

func loadCurrentRetirementStateBinding(directory string, sessionID string, generation uint64) (retirementStateBinding, error) {
	directory = filepath.Clean(directory)
	stateData, stateIdentity, err := readStableRetirementFile(filepath.Join(directory, "state.json"))
	if err != nil {
		return retirementStateBinding{}, err
	}
	var state vfs.SessionState
	if err := decodeStrictRetirementJSON(stateData, &state); err != nil {
		return retirementStateBinding{}, fmt.Errorf("decode retirement binding state: %w", err)
	}
	if state.SessionID != sessionID || state.Generation != generation {
		return retirementStateBinding{}, errors.New("retirement binding state identity changed")
	}
	catalogData, _, err := readStableRetirementFile(filepath.Join(directory, "state-catalog.json"))
	if err != nil {
		return retirementStateBinding{}, err
	}
	var catalog retirementStateCatalog
	if err := decodeStrictRetirementJSON(catalogData, &catalog); err != nil {
		return retirementStateBinding{}, fmt.Errorf("decode retirement binding catalog: %w", err)
	}
	if catalog.Version != 1 || catalog.SessionID != sessionID || catalog.Generation != generation || catalog.Sequence == 0 || catalog.StateSHA256 != stateIdentity.SHA256 || !validRetirementSHA256(catalog.CheckpointSHA256) || filepath.Base(catalog.Checkpoint) != catalog.Checkpoint {
		return retirementStateBinding{}, errors.New("retirement binding catalog does not match state")
	}
	checkpointData, checkpointIdentity, err := readStableRetirementFile(filepath.Join(directory, "state-generations", catalog.Checkpoint))
	if err != nil {
		return retirementStateBinding{}, err
	}
	if checkpointIdentity.SHA256 != catalog.CheckpointSHA256 {
		return retirementStateBinding{}, errors.New("retirement binding checkpoint hash differs from catalog")
	}
	var checkpoint retirementStateCheckpoint
	if err := decodeStrictRetirementJSON(checkpointData, &checkpoint); err != nil {
		return retirementStateBinding{}, fmt.Errorf("decode retirement binding checkpoint: %w", err)
	}
	if err := validateRetirementCheckpoint(directory, checkpoint); err != nil {
		return retirementStateBinding{}, err
	}
	if checkpoint.SessionID != sessionID || checkpoint.Sequence != catalog.Sequence || checkpoint.State != state || checkpoint.StateSHA256 != stateIdentity.SHA256 {
		return retirementStateBinding{}, errors.New("retirement binding checkpoint does not bind current state")
	}
	return retirementStateBinding{
		StateSHA256: stateIdentity.SHA256, CheckpointSequence: catalog.Sequence, CheckpointSHA256: catalog.CheckpointSHA256,
	}, nil
}

func loadRetirementStateProof(store string, pending pendingCanonicalRetirement, request retirementControl) (retirementStateProof, error) {
	stateData, stateIdentity, err := readStableRetirementFile(filepath.Join(pending.Directory, "state.json"))
	if err != nil {
		return retirementStateProof{}, err
	}
	var state vfs.SessionState
	if err := decodeStrictRetirementJSON(stateData, &state); err != nil {
		return retirementStateProof{}, fmt.Errorf("decode retirement state: %w", err)
	}
	activeDirectory := filepath.Join(store, "fs", "sessions", pending.SessionID)
	if err := validateRetirementStateValue(store, activeDirectory, state); err != nil {
		return retirementStateProof{}, err
	}
	if state.SessionID != pending.SessionID || (state.Generation != request.Generation && (!pending.Active || state.Generation != request.Generation+1)) {
		return retirementStateProof{}, errors.New("retirement state identity does not match its request")
	}

	catalogData, _, err := readStableRetirementFile(filepath.Join(pending.Directory, "state-catalog.json"))
	if err != nil {
		return retirementStateProof{}, fmt.Errorf("read retirement state catalog: %w", err)
	}
	var catalog retirementStateCatalog
	if err := decodeStrictRetirementJSON(catalogData, &catalog); err != nil {
		return retirementStateProof{}, fmt.Errorf("decode retirement state catalog: %w", err)
	}
	if catalog.Version != 1 || catalog.SessionID != state.SessionID || catalog.Generation != state.Generation || catalog.Sequence == 0 || !validRetirementSHA256(catalog.StateSHA256) || !validRetirementSHA256(catalog.CheckpointSHA256) || filepath.Base(catalog.Checkpoint) != catalog.Checkpoint || catalog.StateSHA256 != stateIdentity.SHA256 {
		return retirementStateProof{}, errors.New("retirement state catalog does not match state.json")
	}

	checkpoints, err := loadRetirementCheckpointChain(pending.Directory, state.SessionID, activeDirectory, catalog)
	if err != nil {
		return retirementStateProof{}, err
	}
	target := checkpoints[catalog.CheckpointSHA256]
	if target.State != state || target.StateSHA256 != stateIdentity.SHA256 {
		return retirementStateProof{}, errors.New("retirement checkpoint does not bind the current state bytes")
	}
	bound, exists := checkpoints[request.CheckpointSHA256]
	if !exists || bound.Sequence != request.CheckpointSequence || bound.State.Generation != request.Generation || bound.StateSHA256 != request.StateSHA256 {
		return retirementStateProof{}, errors.New("retirement request no longer binds the managed checkpoint lineage")
	}
	requestBindingMatches := state.Generation == request.Generation &&
		catalog.Sequence == request.CheckpointSequence &&
		catalog.CheckpointSHA256 == request.CheckpointSHA256 &&
		catalog.StateSHA256 == request.StateSHA256
	if state.Generation == request.Generation && !requestBindingMatches && catalog.Sequence <= request.CheckpointSequence {
		return retirementStateProof{}, errors.New("managed session checkpoint lineage moved behind the retirement request")
	}
	if err := validateRetirementCurrentArtifacts(store, pending.Directory, activeDirectory, target); err != nil {
		return retirementStateProof{}, err
	}
	if err := validateRetirementDirectoryContents(pending.Directory, activeDirectory, target); err != nil {
		return retirementStateProof{}, err
	}
	return retirementStateProof{State: state, Checkpoint: target, RequestBindingMatches: requestBindingMatches}, nil
}

func validateRetirementStateValue(store string, activeDirectory string, state vfs.SessionState) error {
	if state.Version != 2 || !validSessionID(state.SessionID) || state.Generation == 0 || state.BaseBytes < 0 || !validRetirementSHA256(state.BaseSHA256) || !validRetirementSHA256(state.ManifestSHA256) {
		return errors.New("invalid retirement session state")
	}
	if filepath.Clean(state.ManifestPath) != fold.ManifestPath(store, state.SessionID) {
		return errors.New("retirement state does not reference its canonical manifest")
	}
	for label, path := range map[string]string{"delta": state.DeltaPath, "backing": state.BackingPath} {
		if path == "" && label == "backing" {
			continue
		}
		if !retirementPathWithin(activeDirectory, path) || filepath.Clean(path) == filepath.Clean(activeDirectory) {
			return fmt.Errorf("retirement state contains an unsafe %s path", label)
		}
	}
	if state.NativeSnapshot.Path == "" {
		if state.NativeSnapshot.Bytes != 0 || state.NativeSnapshot.SHA256 != "" {
			return errors.New("retirement state contains partial native snapshot metadata")
		}
	} else if state.NativeSnapshot.Bytes < 0 || !validRetirementSHA256(state.NativeSnapshot.SHA256) || !filepath.IsAbs(state.NativeSnapshot.Path) {
		return errors.New("retirement state contains invalid native snapshot metadata")
	}
	return nil
}

func loadRetirementCheckpointChain(directory string, sessionID string, activeDirectory string, catalog retirementStateCatalog) (map[string]retirementStateCheckpoint, error) {
	generations := filepath.Join(directory, "state-generations")
	info, err := os.Lstat(generations)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("retirement state checkpoint root is not a real directory")
	}
	entries, err := os.ReadDir(generations)
	if err != nil {
		return nil, err
	}
	checkpoints := make(map[string]retirementStateCheckpoint)
	for _, entry := range entries {
		path := filepath.Join(generations, entry.Name())
		entryInfo, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(entry.Name(), ".state-checkpoint-") && strings.HasSuffix(entry.Name(), ".tmp") {
			if !entryInfo.Mode().IsRegular() || entryInfo.Size() != 0 {
				return nil, errors.New("nonempty temporary state checkpoint prevents retirement recovery")
			}
			continue
		}
		data, identity, err := readStableRetirementFile(path)
		if err != nil {
			return nil, err
		}
		var checkpoint retirementStateCheckpoint
		if err := decodeStrictRetirementJSON(data, &checkpoint); err != nil {
			return nil, fmt.Errorf("decode retirement state checkpoint %s: %w", entry.Name(), err)
		}
		if err := validateRetirementCheckpoint(activeDirectory, checkpoint); err != nil {
			return nil, fmt.Errorf("validate retirement state checkpoint %s: %w", entry.Name(), err)
		}
		expectedName := fmt.Sprintf("%020d-%020d-%s.json", checkpoint.State.Generation, checkpoint.Sequence, identity.SHA256)
		if entry.Name() != expectedName || checkpoint.SessionID != sessionID {
			return nil, errors.New("retirement state checkpoint filename or session identity is invalid")
		}
		if _, duplicate := checkpoints[identity.SHA256]; duplicate {
			return nil, errors.New("duplicate retirement state checkpoint identity")
		}
		checkpoints[identity.SHA256] = checkpoint
	}
	target, ok := checkpoints[catalog.CheckpointSHA256]
	if !ok || target.State.Generation != catalog.Generation || target.Sequence != catalog.Sequence || target.StateSHA256 != catalog.StateSHA256 || catalog.Checkpoint != fmt.Sprintf("%020d-%020d-%s.json", target.State.Generation, target.Sequence, catalog.CheckpointSHA256) {
		return nil, errors.New("retirement state checkpoint catalog target is missing or conflicting")
	}
	visited := make(map[string]struct{}, len(checkpoints))
	for digest := catalog.CheckpointSHA256; ; {
		checkpoint, ok := checkpoints[digest]
		if !ok {
			return nil, errors.New("retirement state checkpoint lineage is incomplete")
		}
		if _, duplicate := visited[digest]; duplicate {
			return nil, errors.New("retirement state checkpoint lineage contains a cycle")
		}
		visited[digest] = struct{}{}
		previous := checkpoint.PreviousCheckpointSHA256
		if previous == "" {
			break
		}
		parent, ok := checkpoints[previous]
		if !ok || parent.Sequence >= checkpoint.Sequence || parent.State.Generation > checkpoint.State.Generation {
			return nil, errors.New("retirement state checkpoint lineage is invalid")
		}
		digest = previous
	}
	if len(visited) != len(checkpoints) {
		return nil, errors.New("retirement state checkpoint history is ambiguous")
	}
	return checkpoints, nil
}

func validateRetirementCheckpoint(activeDirectory string, checkpoint retirementStateCheckpoint) error {
	if checkpoint.Version != 1 || checkpoint.SessionID != checkpoint.State.SessionID || checkpoint.Sequence == 0 || !validRetirementSHA256(checkpoint.StateSHA256) || (checkpoint.PreviousCheckpointSHA256 != "" && !validRetirementSHA256(checkpoint.PreviousCheckpointSHA256)) {
		return errors.New("invalid state checkpoint metadata")
	}
	if err := validateRetirementStateValue(filepath.Dir(filepath.Dir(filepath.Dir(activeDirectory))), activeDirectory, checkpoint.State); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(checkpoint.State, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if digestRetirementBytes(encoded) != checkpoint.StateSHA256 {
		return errors.New("state checkpoint hash differs from its state")
	}
	if err := validateRetirementFileIdentity(checkpoint.Manifest); err != nil || filepath.Clean(checkpoint.Manifest.Path) != filepath.Clean(checkpoint.State.ManifestPath) || checkpoint.Manifest.SHA256 != checkpoint.State.ManifestSHA256 {
		return errors.New("invalid manifest checkpoint identity")
	}
	if err := validateRetirementFileIdentity(checkpoint.Delta); err != nil || filepath.Clean(checkpoint.Delta.Path) != filepath.Clean(checkpoint.State.DeltaPath) {
		return errors.New("invalid delta checkpoint identity")
	}
	if checkpoint.State.BackingPath == "" {
		if checkpoint.Backing != nil {
			return errors.New("unexpected backing checkpoint identity")
		}
	} else if checkpoint.Backing == nil || validateRetirementFileIdentity(*checkpoint.Backing) != nil || filepath.Clean(checkpoint.Backing.Path) != filepath.Clean(checkpoint.State.BackingPath) {
		return errors.New("invalid backing checkpoint identity")
	}
	if checkpoint.State.NativeSnapshot.Path == "" {
		if checkpoint.NativeSnapshot != nil {
			return errors.New("unexpected native snapshot checkpoint identity")
		}
	} else if checkpoint.NativeSnapshot == nil || validateRetirementFileIdentity(*checkpoint.NativeSnapshot) != nil || filepath.Clean(checkpoint.NativeSnapshot.Path) != filepath.Clean(checkpoint.State.NativeSnapshot.Path) || checkpoint.NativeSnapshot.Bytes != checkpoint.State.NativeSnapshot.Bytes || checkpoint.NativeSnapshot.SHA256 != checkpoint.State.NativeSnapshot.SHA256 {
		return errors.New("invalid native snapshot checkpoint identity")
	}
	return nil
}

func validateRetirementCurrentArtifacts(store string, directory string, activeDirectory string, checkpoint retirementStateCheckpoint) error {
	manifestIdentity, err := hashStableRetirementFile(checkpoint.Manifest.Path)
	if err != nil || manifestIdentity.Bytes != checkpoint.Manifest.Bytes || manifestIdentity.SHA256 != checkpoint.Manifest.SHA256 {
		return errors.New("manifest bytes do not match the retirement state checkpoint")
	}
	reference := storage.ManagedSessionReference{
		StoreDir: store, SessionID: checkpoint.State.SessionID, ManifestPath: checkpoint.State.ManifestPath,
		ManifestSHA256: checkpoint.State.ManifestSHA256, BaseBytes: checkpoint.State.BaseBytes, BaseSHA256: checkpoint.State.BaseSHA256,
	}
	if err := storage.ValidateManagedSessionReference(reference); err != nil {
		return fmt.Errorf("validate exact retirement manifest: %w", err)
	}
	for label, expected := range map[string]retirementFileIdentity{"delta": checkpoint.Delta} {
		actualPath, err := relocatedRetirementStatePath(directory, activeDirectory, expected.Path)
		if err != nil {
			return err
		}
		identity, err := hashStableRetirementFile(actualPath)
		if err != nil || identity.Bytes != expected.Bytes || identity.SHA256 != expected.SHA256 {
			return fmt.Errorf("retirement %s bytes do not match their checkpoint", label)
		}
	}
	if checkpoint.Backing != nil {
		actualPath, err := relocatedRetirementStatePath(directory, activeDirectory, checkpoint.Backing.Path)
		if err != nil {
			return err
		}
		identity, err := hashStableRetirementFile(actualPath)
		if err != nil || identity.Bytes != checkpoint.Backing.Bytes || identity.SHA256 != checkpoint.Backing.SHA256 {
			return errors.New("retirement backing bytes do not match their checkpoint")
		}
	}
	return validateTerminalRetirementJournal(directory, checkpoint.State.SessionID)
}

func validateRetirementDirectoryContents(directory string, activeDirectory string, checkpoint retirementStateCheckpoint) error {
	knownFiles := map[string]struct{}{
		"state.json": {}, "state-catalog.json": {}, "writer.lease": {},
		retirementRequestFilename: {}, retirementAcknowledgementFilename: {},
		"mounted.json": {}, "journal.jsonl": {}, vfs.NativeRetirementFilename: {},
	}
	knownDirectories := map[string]struct{}{
		"state-generations": {}, "state-orphans": {}, "leases": {}, "retained-native": {},
	}
	for _, expected := range []*retirementFileIdentity{&checkpoint.Delta, checkpoint.Backing} {
		if expected == nil {
			continue
		}
		actual, err := relocatedRetirementStatePath(directory, activeDirectory, expected.Path)
		if err != nil {
			return err
		}
		if filepath.Dir(actual) != filepath.Clean(directory) {
			return errors.New("nested managed session data is not supported by retirement recovery")
		}
		knownFiles[filepath.Base(actual)] = struct{}{}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if _, ok := knownFiles[entry.Name()]; ok {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("known retirement file has an unsafe type: %s", entry.Name())
			}
			continue
		}
		if _, ok := knownDirectories[entry.Name()]; ok {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("known retirement directory has an unsafe type: %s", entry.Name())
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unknown retirement content has an unsafe type: %s", entry.Name())
		}
		if info.Mode().IsRegular() && info.Size() == 0 {
			continue
		}
		if info.IsDir() {
			nonempty, err := retirementTreeContainsContent(path)
			if err != nil {
				return err
			}
			if !nonempty {
				continue
			}
		}
		return fmt.Errorf("unknown nonempty content prevents retirement recovery: %s", entry.Name())
	}
	return nil
}

func cleanupStaleRetirementControlTemporaries(directory string) error {
	directory = filepath.Clean(directory)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	controls := []string{retirementRequestFilename, retirementAcknowledgementFilename, "mounted.json"}
	removed := false
	for _, entry := range entries {
		matched := false
		for _, control := range controls {
			if strings.HasPrefix(entry.Name(), "."+control+"-") && strings.HasSuffix(entry.Name(), ".tmp") {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maximumRetirementJSONBytes {
			return errors.New("stale retirement control temporary is not a bounded regular file")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncRetirementDirectory(directory)
	}
	return nil
}

func validateTerminalRetirementJournal(directory string, sessionID string) error {
	path := filepath.Join(directory, "journal.jsonl")
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	if statErr != nil {
		return statErr
	}
	if !info.Mode().IsRegular() {
		return errors.New("retirement journal is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	latest := make(map[string]vfs.JournalRecord)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var record vfs.JournalRecord
		if err := decodeStrictRetirementJSON(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("decode retirement journal: %w", err)
		}
		if record.OperationID == "" || record.SessionID != sessionID {
			return errors.New("retirement journal does not match its session")
		}
		latest[record.OperationID] = record
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, record := range latest {
		if record.Phase != "complete" && record.Phase != "rolled-back" {
			return errors.New("pending session journal operation prevents retirement recovery")
		}
	}
	return nil
}

func retirementReadersActive(directory string) (bool, error) {
	root := filepath.Join(directory, "leases")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("retirement reader lease root is not a real directory")
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "generation-") {
			return false, fmt.Errorf("unknown retirement reader lease entry: %s", entry.Name())
		}
		active, err := storage.DirectoryHasActiveLease(path, false)
		if err != nil {
			return false, err
		}
		if active {
			return true, nil
		}
	}
	return false, nil
}

func validateRetirementSnapshot(
	store string,
	nativeRoot string,
	pending pendingCanonicalRetirement,
	state vfs.SessionState,
	targetPath string,
	request retirementControl,
	targetMatches bool,
	acknowledged bool,
) (retirementSnapshotProof, error) {
	if state.NativeSnapshot.Path == "" {
		return retirementSnapshotProof{}, validateRetainedNativeTreeEmpty(pending.Directory)
	}
	if acknowledged && !isHiddenRetirementSnapshot(store, state.SessionID, state.NativeSnapshot.Path) {
		if err := validateRetirementPathComponents(nativeRoot, state.NativeSnapshot.Path, true); err != nil {
			return retirementSnapshotProof{}, fmt.Errorf("validate committed legacy native snapshot path: %w", err)
		}
		retiredPath, err := canonicalRetiredSnapshotPath(store, nativeRoot, state.SessionID, state.NativeSnapshot.Path, pending.Directory)
		if err != nil {
			return retirementSnapshotProof{}, err
		}
		retiredExists, err := retirementFileMatchesNative(retiredPath, state.NativeSnapshot)
		if err != nil {
			return retirementSnapshotProof{}, fmt.Errorf("verify retained committed native snapshot: %w", err)
		}
		if err := validateRetainedNativeTree(pending.Directory, retiredPath, retiredExists); err != nil {
			return retirementSnapshotProof{}, err
		}
		originalExists, info, err := retirementPathStatus(state.NativeSnapshot.Path)
		if err != nil {
			return retirementSnapshotProof{}, err
		}
		if originalExists && !info.Mode().IsRegular() {
			return retirementSnapshotProof{}, errors.New("committed legacy native snapshot path is not a regular file")
		}
		return retirementSnapshotProof{
			OriginalPath: state.NativeSnapshot.Path,
			RetiredPath:  retiredPath,
			AtOriginal:   originalExists,
			AtRetired:    retiredExists,
		}, nil
	}
	if filepath.Clean(state.NativeSnapshot.Path) == filepath.Clean(targetPath) {
		if !targetMatches || !acknowledged && (state.NativeSnapshot.Bytes != request.Bytes || state.NativeSnapshot.SHA256 != request.SHA256) {
			return retirementSnapshotProof{}, errors.New("rollback target cannot prove the original native snapshot")
		}
		if err := validateRetainedNativeTreeEmpty(pending.Directory); err != nil {
			return retirementSnapshotProof{}, err
		}
		return retirementSnapshotProof{OriginalPath: state.NativeSnapshot.Path, AtOriginal: true}, nil
	}
	retiredPath, err := canonicalRetiredSnapshotPath(store, nativeRoot, state.SessionID, state.NativeSnapshot.Path, pending.Directory)
	if err != nil {
		return retirementSnapshotProof{}, err
	}
	snapshotRoot, err := canonicalNativeSnapshotRoot(store, nativeRoot, state.SessionID, state.NativeSnapshot.Path)
	if err != nil {
		return retirementSnapshotProof{}, err
	}
	if err := validateRetirementPathComponents(snapshotRoot, state.NativeSnapshot.Path, true); err != nil {
		return retirementSnapshotProof{}, fmt.Errorf("validate native snapshot path: %w", err)
	}
	originalExists, originalErr := retirementFileMatchesNative(state.NativeSnapshot.Path, state.NativeSnapshot)
	retiredExists, retiredErr := retirementFileMatchesNative(retiredPath, state.NativeSnapshot)
	if originalErr != nil {
		return retirementSnapshotProof{}, fmt.Errorf("verify original native snapshot: %w", originalErr)
	}
	if retiredErr != nil {
		return retirementSnapshotProof{}, fmt.Errorf("verify retired native snapshot: %w", retiredErr)
	}
	if originalExists == retiredExists {
		if originalExists {
			return retirementSnapshotProof{}, errors.New("original and retired native snapshots both exist")
		}
		return retirementSnapshotProof{}, errors.New("exact native snapshot is missing from both recovery locations")
	}
	if err := validateRetainedNativeTree(pending.Directory, retiredPath, retiredExists); err != nil {
		return retirementSnapshotProof{}, err
	}
	if err := validateSnapshotSidecarPair(state.NativeSnapshot.Path, retiredPath); err != nil {
		return retirementSnapshotProof{}, err
	}
	return retirementSnapshotProof{
		OriginalPath: state.NativeSnapshot.Path, RetiredPath: retiredPath,
		AtOriginal: originalExists, AtRetired: retiredExists,
	}, nil
}

func completeCanonicalRetirement(
	ctx context.Context,
	home string,
	store string,
	mount string,
	nativeRoot string,
	plan *canonicalRetirementPlan,
	hook canonicalRetirementRecoveryHook,
	handoff canonicalRetirementOwnerHandoff,
) (bool, error) {
	if plan.Acknowledged {
		if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseBeforeCommittedMove); err != nil {
			return false, err
		}
	}
	if err := revalidateCanonicalRetirementPlan(home, store, mount, nativeRoot, plan); err != nil {
		return false, err
	}
	plan.Restore = canonicalRetirementPlanRequiresRestore(plan)
	if plan.Restore {
		return true, restoreCanonicalRetirement(ctx, store, nativeRoot, plan, hook)
	}
	if !plan.Acknowledged {
		current, err := canonicalRetirementRouteStillMatches(home, mount, plan.Pending.SessionID, plan.Request.Route)
		if err != nil {
			return false, err
		}
		if !current {
			return true, restoreCanonicalRetirement(ctx, store, nativeRoot, plan, hook)
		}
		if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseBeforeOwnerHandoff); err != nil {
			return false, err
		}
		if err := revalidateCanonicalRetirementPlan(home, store, mount, nativeRoot, plan); err != nil {
			return false, err
		}
		if !plan.Acknowledged {
			plan.Restore = canonicalRetirementPlanRequiresRestore(plan)
			current, err = canonicalRetirementRouteStillMatches(home, mount, plan.Pending.SessionID, plan.Request.Route)
			if err != nil {
				return false, err
			}
			if plan.Restore || !current {
				return true, restoreCanonicalRetirement(ctx, store, nativeRoot, plan, hook)
			}
		}
	}
	if !plan.Acknowledged {
		commit := func() error {
			if _, err := vfs.LoadSessionDeletion(store, plan.Pending.SessionID); err == nil {
				return errors.New("explicit deletion tombstone supersedes retirement cutover")
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			binding, err := loadCurrentRetirementStateBinding(plan.Pending.Directory, plan.Pending.SessionID, plan.Request.Generation)
			if err != nil {
				return publishRetirementCutoverRejectionAt(plan.Pending.Directory, plan.Request, "managed checkpoint changed before retirement cutover")
			}
			if binding.StateSHA256 != plan.Request.StateSHA256 || binding.CheckpointSequence != plan.Request.CheckpointSequence || binding.CheckpointSHA256 != plan.Request.CheckpointSHA256 {
				return publishRetirementCutoverRejectionAt(plan.Pending.Directory, plan.Request, "managed session advanced after retirement request publication")
			}
			matches, err := canonicalRetirementTargetStillMatches(nativeRoot, plan.TargetPath, plan.Request)
			if err != nil || !matches {
				return publishRetirementCutoverRejectionAt(plan.Pending.Directory, plan.Request, "native rollback target changed before retirement cutover")
			}
			return writeRetirementAcknowledgementAt(plan.Pending.Directory, plan.Request)
		}
		if handoff != nil {
			if err := handoff(plan.Pending.SessionID, plan.Request.Route, commit); err != nil {
				if errors.Is(err, errRetirementCutoverRejected) {
					plan.Rejected = true
					plan.Rejection = err.Error()
					return true, restoreCanonicalRetirement(ctx, store, nativeRoot, plan, hook)
				}
				if errors.Is(err, mountfs.ErrManagedSessionRouteChanged) {
					if rejectErr := ensureRetirementRejectionAt(plan.Pending.Directory, plan.Request, "managed session route changed before retirement cutover"); rejectErr != nil {
						return false, errors.Join(err, rejectErr)
					}
					plan.Rejected = true
					plan.Rejection = "managed session route changed before retirement cutover"
					return true, restoreCanonicalRetirement(ctx, store, nativeRoot, plan, hook)
				}
				return false, fmt.Errorf("detach completed canonical retirement owner: %w", err)
			}
		} else if err := commit(); err != nil {
			if errors.Is(err, errRetirementCutoverRejected) {
				plan.Rejected = true
				plan.Rejection = err.Error()
				return true, restoreCanonicalRetirement(ctx, store, nativeRoot, plan, hook)
			}
			return false, fmt.Errorf("publish canonical retirement cutover acknowledgement: %w", err)
		}
		plan.Acknowledged = true
		if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseOwnerDetached); err != nil {
			return false, err
		}
	} else if handoff != nil {
		if err := handoff(plan.Pending.SessionID, "", nil); err != nil {
			return false, fmt.Errorf("detach completed canonical retirement owner: %w", err)
		}
		if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseOwnerDetached); err != nil {
			return false, err
		}
	}
	if plan.Pending.Active {
		retiredDirectory, err := retireManagedStateForRecovery(store, plan.Pending.SessionID, plan.Pending.Directory)
		if err != nil {
			return false, err
		}
		plan.Pending.Directory = retiredDirectory
		plan.Pending.Active = false
		if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseStateRetired); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := completeNativeSnapshotRetirement(store, nativeRoot, plan); err != nil {
		return false, err
	}
	if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseNativeRetired); err != nil {
		return false, err
	}
	if err := removeManagedSessionRegistryEntryIfPresent(store, plan.Pending.SessionID); err != nil {
		return false, fmt.Errorf("remove completed retirement from durable managed session registry: %w", err)
	}
	return false, clearRetirementRequestLast(plan.Pending.Directory, plan.Request, true, hook, plan.Pending.SessionID)
}

func canonicalRetirementPlanRequiresRestore(plan *canonicalRetirementPlan) bool {
	return !plan.Acknowledged && (plan.Rejected || !plan.StateProof.RequestBindingMatches || plan.StateProof.State.Generation == plan.Request.Generation+1 || !plan.RouteMatches || !plan.TargetMatches)
}

func canonicalRetirementRouteStillMatches(home string, mount string, sessionID string, expectedRoute string) (bool, error) {
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return false, fmt.Errorf("refresh Codex metadata before retirement completion: %w", err)
	}
	return canonicalRetirementRouteMatchesSessions(home, mount, sessions, sessionID, expectedRoute)
}

func canonicalRetirementRouteMatchesSessions(home string, mount string, sessions []codex.Session, sessionID string, expectedRoute string) (bool, error) {
	seen := false
	matched := false
	for _, session := range sessions {
		if session.ID != sessionID {
			continue
		}
		if seen {
			return false, errors.New("Codex metadata contains duplicate session IDs")
		}
		seen = true
		route, routeErr := canonicalNamespaceRoute(home, mount, session.RolloutPath)
		matched = routeErr == nil && route == expectedRoute
	}
	return seen && matched, nil
}

func canonicalRetirementTargetStillMatches(nativeRoot string, targetPath string, request retirementControl) (bool, error) {
	if err := validateRetirementPathComponents(nativeRoot, targetPath, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		// Unsafe or unreadable path components are not authoritative rollback
		// targets. Preserve the managed transaction and use exact snapshot proof
		// for compensation instead of following or replacing them.
		return false, nil
	}
	identity, err := hashStableRetirementFile(targetPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, nil
	}
	return identity.Bytes == request.Bytes && identity.SHA256 == request.SHA256, nil
}

func hashStableCanonicalNativeFile(nativeRoot string, path string) (vfs.NativeFile, error) {
	if err := validateRetirementPathComponents(nativeRoot, path, false); err != nil {
		return vfs.NativeFile{}, err
	}
	identity, err := hashStableRetirementFile(path)
	if err != nil {
		return vfs.NativeFile{}, err
	}
	return vfs.NativeFile{Path: filepath.Clean(path), Bytes: identity.Bytes, SHA256: identity.SHA256}, nil
}

func canonicalNativeFileStillMatches(nativeRoot string, path string, expected vfs.NativeFile) bool {
	actual, err := hashStableCanonicalNativeFile(nativeRoot, path)
	return err == nil && actual.Path == filepath.Clean(expected.Path) && actual.Bytes == expected.Bytes && actual.SHA256 == expected.SHA256
}

func restoreCanonicalRetirement(ctx context.Context, store string, nativeRoot string, plan *canonicalRetirementPlan, hook canonicalRetirementRecoveryHook) error {
	if plan.Acknowledged {
		return errors.New("acknowledged retirement cannot be restored to managed authority")
	}
	if err := ensureRetirementRejectionAt(plan.Pending.Directory, plan.Request, retirementRejectionReason(plan)); err != nil {
		return err
	}
	plan.Rejected = true
	if err := restoreNativeSnapshotForRetirement(plan); err != nil {
		return err
	}
	if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseNativeRestored); err != nil {
		return err
	}
	activeDirectory := filepath.Join(store, "fs", "sessions", plan.Pending.SessionID)
	if !plan.Pending.Active {
		if _, err := os.Lstat(activeDirectory); err == nil {
			return errors.New("active session target appeared during retirement restoration")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(plan.Pending.Directory, activeDirectory); err != nil {
			return fmt.Errorf("restore retired session state: %w", err)
		}
		if err := syncRetirementDirectories(filepath.Dir(plan.Pending.Directory), filepath.Dir(activeDirectory)); err != nil {
			return err
		}
		plan.Pending.Directory = activeDirectory
		plan.Pending.Active = true
		if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseStateRestored); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := vfs.LoadSessionState(filepath.Join(activeDirectory, "state.json"))
	if err != nil {
		return fmt.Errorf("validate restored managed state: %w", err)
	}
	if state.Generation == plan.Request.Generation {
		state, err = vfs.RepublishSessionStateWithWriterLease(filepath.Join(activeDirectory, "state.json"))
		if err != nil {
			return fmt.Errorf("republish restored managed state: %w", err)
		}
		if err := runCanonicalRetirementHook(hook, plan.Pending.SessionID, retirementPhaseStateRepublished); err != nil {
			return err
		}
	}
	if state.SessionID != plan.Pending.SessionID || state.Generation != plan.Request.Generation+1 || state.ManifestPath != plan.StateProof.State.ManifestPath || state.ManifestSHA256 != plan.StateProof.State.ManifestSHA256 {
		return errors.New("restored managed state did not advance from the retirement request")
	}
	if err := upsertManagedSessionRegistryEntryIfPresent(store, state.SessionID, state.Generation); err != nil {
		return fmt.Errorf("restore managed session registry entry: %w", err)
	}
	return clearRetirementRequestLast(activeDirectory, plan.Request, false, hook, plan.Pending.SessionID)
}

func retirementRejectionReason(plan *canonicalRetirementPlan) string {
	if plan.Rejection != "" {
		return plan.Rejection
	}
	if !plan.StateProof.RequestBindingMatches || plan.StateProof.State.Generation == plan.Request.Generation+1 {
		return "managed session advanced after retirement request publication"
	}
	if !plan.RouteMatches {
		return "retirement request no longer matches the current session route"
	}
	if !plan.TargetMatches {
		return "native rollback target is unavailable or changed"
	}
	return "retirement cutover was not authorized"
}

func ensureRetirementRejectionAt(directory string, request retirementControl, message string) error {
	status, err := validateRetirementAcknowledgement(directory, request)
	if err != nil {
		return err
	}
	if status.Acknowledged {
		return errors.New("positive retirement acknowledgement already committed")
	}
	if status.Rejected {
		return nil
	}
	rejected := request
	rejected.Error = message
	return writeRetirementAcknowledgementAt(directory, rejected)
}

func publishRetirementCutoverRejectionAt(directory string, request retirementControl, message string) error {
	if err := ensureRetirementRejectionAt(directory, request, message); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", errRetirementCutoverRejected, message)
}

func revalidateCanonicalRetirementPlan(home string, store string, mount string, nativeRoot string, plan *canonicalRetirementPlan) error {
	request, err := loadRetirementRequestAt(plan.Pending.Directory)
	if err != nil || request != plan.Request {
		return errors.New("retirement request changed before recovery mutation")
	}
	proof, err := loadRetirementStateProof(store, plan.Pending, plan.Request)
	if err != nil {
		return err
	}
	acknowledgement, err := validateRetirementAcknowledgement(plan.Pending.Directory, plan.Request)
	if err != nil {
		return err
	}
	if acknowledgement.Acknowledged && !proof.RequestBindingMatches {
		return errors.New("acknowledged retirement managed checkpoint changed after cutover")
	}
	plan.Acknowledged = acknowledgement.Acknowledged
	plan.Rejected = acknowledgement.Rejected
	plan.Rejection = acknowledgement.Error
	var targetMatches bool
	if acknowledgement.Acknowledged {
		targetMatches = true
	} else {
		sessions, err := codex.LoadSessions(home)
		if err != nil {
			return fmt.Errorf("refresh Codex metadata at retirement mutation boundary: %w", err)
		}
		plan.CurrentRoute = ""
		plan.RouteMatches = false
		var currentSession *codex.Session
		for index := range sessions {
			if sessions[index].ID != plan.Pending.SessionID {
				continue
			}
			if currentSession != nil {
				return errors.New("Codex metadata contains duplicate session IDs")
			}
			currentSession = &sessions[index]
		}
		if currentSession != nil {
			currentRoute, routeErr := canonicalNamespaceRoute(home, mount, currentSession.RolloutPath)
			if routeErr == nil {
				plan.CurrentRoute = currentRoute
				plan.RouteMatches = currentRoute == plan.Request.Route
			}
		}
		targetMatches, err = canonicalRetirementTargetStillMatches(nativeRoot, plan.TargetPath, plan.Request)
		if err != nil {
			return err
		}
	}
	snapshot, err := validateRetirementSnapshot(store, nativeRoot, plan.Pending, proof.State, plan.TargetPath, plan.Request, targetMatches, acknowledgement.Acknowledged)
	if err != nil {
		return err
	}
	plan.StateProof = proof
	plan.Snapshot = snapshot
	plan.TargetMatches = targetMatches
	plan.Restore = canonicalRetirementPlanRequiresRestore(plan)
	return nil
}

func retireManagedStateForRecovery(store string, sessionID string, activeDirectory string) (string, error) {
	fsRoot := filepath.Join(filepath.Clean(store), "fs")
	if err := requireRealRetirementDirectory(fsRoot); err != nil {
		return "", err
	}
	retiredRoot := filepath.Join(fsRoot, "retired")
	if err := ensureRetirementDirectory(fsRoot, retiredRoot); err != nil {
		return "", err
	}
	for suffix := int64(1); suffix < 1000; suffix++ {
		target := filepath.Join(retiredRoot, fmt.Sprintf("%s-%d", sessionID, retirementRecoveryTimestamp()+suffix))
		if _, err := os.Lstat(target); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := os.Rename(activeDirectory, target); err != nil {
			return "", err
		}
		if err := syncRetirementDirectories(filepath.Dir(activeDirectory), retiredRoot); err != nil {
			return "", err
		}
		return target, nil
	}
	return "", errors.New("could not allocate a non-conflicting retired state path")
}

var retirementRecoveryTimestamp = func() int64 { return time.Now().UnixNano() }

func completeNativeSnapshotRetirement(store string, nativeRoot string, plan *canonicalRetirementPlan) error {
	state := plan.StateProof.State
	if state.NativeSnapshot.Path == "" || filepath.Clean(state.NativeSnapshot.Path) == filepath.Clean(plan.TargetPath) {
		return nil
	}
	if !isHiddenRetirementSnapshot(store, state.SessionID, state.NativeSnapshot.Path) {
		// Legacy snapshots live in the canonical native namespace and can become
		// the current route after an archive/unarchive rename. Leave them in place;
		// later exact-proof GC may reclaim them without racing live native I/O.
		return nil
	}
	retiredPath, err := canonicalRetiredSnapshotPath(store, nativeRoot, state.SessionID, state.NativeSnapshot.Path, plan.Pending.Directory)
	if err != nil {
		return err
	}
	if err := ensureRetirementDirectory(plan.Pending.Directory, filepath.Dir(retiredPath)); err != nil {
		return err
	}
	originalExists, originalErr := retirementFileMatchesNative(state.NativeSnapshot.Path, state.NativeSnapshot)
	retiredExists, retiredErr := retirementFileMatchesNative(retiredPath, state.NativeSnapshot)
	if originalErr != nil || retiredErr != nil || originalExists == retiredExists {
		return errors.Join(originalErr, retiredErr, errors.New("native snapshot recovery locations are ambiguous"))
	}
	if originalExists {
		if err := os.Rename(state.NativeSnapshot.Path, retiredPath); err != nil {
			return err
		}
		if err := syncRetirementDirectories(filepath.Dir(state.NativeSnapshot.Path), filepath.Dir(retiredPath)); err != nil {
			return err
		}
	}
	if err := moveSnapshotSidecar(state.NativeSnapshot.Path, retiredPath); err != nil {
		return err
	}
	hiddenRoot := filepath.Join(store, "fs", "snapshots", state.SessionID)
	if retirementPathWithin(hiddenRoot, state.NativeSnapshot.Path) {
		_ = os.Remove(hiddenRoot)
	}
	return nil
}

func isHiddenRetirementSnapshot(store string, sessionID string, snapshotPath string) bool {
	return filepath.Clean(snapshotPath) == filepath.Join(filepath.Clean(store), "fs", "snapshots", sessionID, "native.jsonl")
}

func restoreNativeSnapshotForRetirement(plan *canonicalRetirementPlan) error {
	state := plan.StateProof.State
	if state.NativeSnapshot.Path == "" || filepath.Clean(state.NativeSnapshot.Path) == filepath.Clean(plan.TargetPath) {
		return nil
	}
	originalExists, originalErr := retirementFileMatchesNative(state.NativeSnapshot.Path, state.NativeSnapshot)
	retiredExists, retiredErr := retirementFileMatchesNative(plan.Snapshot.RetiredPath, state.NativeSnapshot)
	if originalErr != nil || retiredErr != nil || originalExists == retiredExists {
		return errors.Join(originalErr, retiredErr, errors.New("native snapshot restoration locations are ambiguous"))
	}
	if retiredExists {
		if err := os.MkdirAll(filepath.Dir(state.NativeSnapshot.Path), 0o700); err != nil {
			return err
		}
		if err := os.Rename(plan.Snapshot.RetiredPath, state.NativeSnapshot.Path); err != nil {
			return err
		}
		if err := syncRetirementDirectories(filepath.Dir(plan.Snapshot.RetiredPath), filepath.Dir(state.NativeSnapshot.Path)); err != nil {
			return err
		}
	}
	return moveSnapshotSidecar(plan.Snapshot.RetiredPath, state.NativeSnapshot.Path)
}

func moveSnapshotSidecar(sourcePath string, targetPath string) error {
	source := filepath.Join(filepath.Dir(sourcePath), "._"+filepath.Base(sourcePath))
	target := filepath.Join(filepath.Dir(targetPath), "._"+filepath.Base(targetPath))
	sourceExists, sourceInfo, err := retirementPathStatus(source)
	if err != nil {
		return err
	}
	targetExists, targetInfo, err := retirementPathStatus(target)
	if err != nil {
		return err
	}
	if sourceExists && targetExists {
		return errors.New("native snapshot sidecar exists at both recovery locations")
	}
	for _, item := range []struct {
		exists bool
		info   os.FileInfo
	}{{sourceExists, sourceInfo}, {targetExists, targetInfo}} {
		if item.exists && !item.info.Mode().IsRegular() {
			return errors.New("native snapshot sidecar is not a regular file")
		}
	}
	if !sourceExists {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return syncRetirementDirectories(filepath.Dir(source), filepath.Dir(target))
}

func clearRetirementRequestLast(directory string, request retirementControl, requirePositiveAcknowledgement bool, hook canonicalRetirementRecoveryHook, sessionID string) error {
	current, err := loadRetirementRequestAt(directory)
	if err != nil || current != request {
		return errors.New("retirement request changed before completion")
	}
	status, err := validateRetirementAcknowledgement(directory, request)
	if err != nil {
		return err
	}
	if requirePositiveAcknowledgement && !status.Acknowledged {
		return errors.New("positive retirement acknowledgement is required before committed completion")
	}
	if !requirePositiveAcknowledgement && !status.Rejected {
		return errors.New("rejected retirement acknowledgement is required before compensation completion")
	}
	if err := runCanonicalRetirementHook(hook, sessionID, retirementPhaseBeforeRequestCleared); err != nil {
		return err
	}
	current, err = loadRetirementRequestAt(directory)
	if err != nil || current != request {
		return errors.New("retirement request changed before final completion")
	}
	status, err = validateRetirementAcknowledgement(directory, request)
	if err != nil || requirePositiveAcknowledgement && !status.Acknowledged || !requirePositiveAcknowledgement && !status.Rejected {
		return errors.New("retirement acknowledgement changed before final completion")
	}
	if err := os.Remove(filepath.Join(directory, retirementRequestFilename)); err != nil {
		return err
	}
	if err := syncRetirementDirectory(directory); err != nil {
		return err
	}
	if err := runCanonicalRetirementHook(hook, sessionID, retirementPhaseRequestCleared); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(directory, retirementAcknowledgementFilename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRetirementDirectory(directory)
}

func runCanonicalRetirementHook(hook canonicalRetirementRecoveryHook, sessionID string, phase string) error {
	if hook == nil {
		return nil
	}
	return hook(sessionID, phase)
}

func canonicalRetiredSnapshotPath(store string, nativeRoot string, sessionID string, snapshotPath string, retiredDirectory string) (string, error) {
	snapshotPath = filepath.Clean(snapshotPath)
	var relative string
	if candidate, err := filepath.Rel(filepath.Clean(nativeRoot), snapshotPath); err == nil && candidate != "." && candidate != ".." && !strings.HasPrefix(candidate, ".."+string(filepath.Separator)) {
		relative = candidate
	} else {
		hiddenRoot := filepath.Join(store, "fs", "snapshots", sessionID)
		candidate, hiddenErr := filepath.Rel(hiddenRoot, snapshotPath)
		if hiddenErr != nil || candidate != "native.jsonl" || filepath.Base(snapshotPath) != "native.jsonl" {
			return "", errors.New("native snapshot is outside canonical retirement roots")
		}
		relative = filepath.Join("store-snapshot", "native.jsonl")
	}
	target := filepath.Join(retiredDirectory, "retained-native", relative)
	if !retirementPathWithin(filepath.Join(retiredDirectory, "retained-native"), target) {
		return "", errors.New("retired native snapshot path escapes its transaction")
	}
	return target, nil
}

func canonicalNativeSnapshotRoot(store string, nativeRoot string, sessionID string, snapshotPath string) (string, error) {
	snapshotPath = filepath.Clean(snapshotPath)
	nativeRoot = filepath.Clean(nativeRoot)
	if retirementPathWithin(nativeRoot, snapshotPath) && snapshotPath != nativeRoot {
		return nativeRoot, nil
	}
	snapshotsRoot := filepath.Join(filepath.Clean(store), "fs", "snapshots")
	hiddenRoot := filepath.Join(snapshotsRoot, sessionID)
	if filepath.Clean(snapshotPath) == filepath.Join(hiddenRoot, "native.jsonl") {
		return snapshotsRoot, nil
	}
	return "", errors.New("native snapshot is outside canonical retirement roots")
}

func validateRetirementPathComponents(root string, path string, allowMissing bool) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !filepath.IsAbs(root) || !retirementPathWithin(root, path) || root == path {
		return errors.New("retirement path is outside its canonical root")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("retirement path root is not a real directory")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	current := root
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && allowMissing {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("retirement path contains a symlink: %s", current)
		}
		if index < len(components)-1 && !info.IsDir() {
			return fmt.Errorf("retirement path ancestor is not a directory: %s", current)
		}
	}
	return nil
}

func validateRetainedNativeTreeEmpty(directory string) error {
	root := filepath.Join(directory, "retained-native")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("retained native root is not a real directory")
	}
	nonempty, err := retirementTreeContainsContent(root)
	if err != nil {
		return err
	}
	if nonempty {
		return errors.New("unexpected retained native content prevents retirement recovery")
	}
	return nil
}

func validateRetainedNativeTree(directory string, expectedPath string, expectedExists bool) error {
	root := filepath.Join(directory, "retained-native")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if expectedExists {
			return errors.New("retained native root is missing")
		}
		return nil
	} else if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("retained native root is not a real directory")
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("retained native tree contains a symlink")
		}
		if info.IsDir() || filepath.Clean(path) == filepath.Clean(expectedPath) || filepath.Clean(path) == filepath.Join(filepath.Dir(expectedPath), "._"+filepath.Base(expectedPath)) {
			return nil
		}
		if info.Mode().IsRegular() && info.Size() == 0 {
			return nil
		}
		return fmt.Errorf("unknown nonempty retained native content: %s", path)
	})
}

func validateSnapshotSidecarPair(originalPath string, retiredPath string) error {
	original := filepath.Join(filepath.Dir(originalPath), "._"+filepath.Base(originalPath))
	retired := filepath.Join(filepath.Dir(retiredPath), "._"+filepath.Base(retiredPath))
	originalExists, originalInfo, err := retirementPathStatus(original)
	if err != nil {
		return err
	}
	retiredExists, retiredInfo, err := retirementPathStatus(retired)
	if err != nil {
		return err
	}
	if originalExists && retiredExists {
		return errors.New("native snapshot sidecar exists at both recovery locations")
	}
	if (originalExists && !originalInfo.Mode().IsRegular()) || (retiredExists && !retiredInfo.Mode().IsRegular()) {
		return errors.New("native snapshot sidecar is not a regular file")
	}
	return nil
}

func retirementFileMatchesNative(path string, expected vfs.NativeFile) (bool, error) {
	identity, err := hashStableRetirementFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if identity.Bytes != expected.Bytes || identity.SHA256 != expected.SHA256 {
		return false, errors.New("native snapshot bytes differ from session state")
	}
	return true, nil
}

func relocatedRetirementStatePath(directory string, activeDirectory string, recorded string) (string, error) {
	relative, err := filepath.Rel(filepath.Clean(activeDirectory), filepath.Clean(recorded))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("session data path escapes its canonical active directory")
	}
	return filepath.Join(filepath.Clean(directory), relative), nil
}

func retirementTreeContainsContent(root string) (bool, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	if !info.IsDir() {
		return !info.Mode().IsRegular() || info.Size() != 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		nonempty, err := retirementTreeContainsContent(filepath.Join(root, entry.Name()))
		if err != nil {
			return false, err
		}
		if nonempty {
			return true, nil
		}
	}
	return false, nil
}

func ensureRetirementDirectory(root string, directory string) error {
	root = filepath.Clean(root)
	directory = filepath.Clean(directory)
	if !retirementPathWithin(root, directory) {
		return errors.New("retirement directory escapes its transaction root")
	}
	relative, err := filepath.Rel(root, directory)
	if err != nil {
		return err
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		next := filepath.Join(current, component)
		if err := os.Mkdir(next, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := requireRealRetirementDirectory(next); err != nil {
			return err
		}
		if err := syncRetirementDirectory(current); err != nil {
			return err
		}
		current = next
	}
	return nil
}

func requireRealRetirementDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("retirement path is not a real directory")
	}
	return nil
}

func retirementPathStatus(path string) (bool, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil, nil
	}
	return err == nil, info, err
}

func readStableRetirementFile(path string) ([]byte, retirementFileIdentity, error) {
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, retirementFileIdentity{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, retirementFileIdentity{}, errors.New("retirement proof path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, retirementFileIdentity{}, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, retirementFileIdentity{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, retirementFileIdentity{}, errors.New("retirement proof file changed while it was opened")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximumRetirementJSONBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, retirementFileIdentity{}, err
	}
	if len(data) > maximumRetirementJSONBytes {
		return nil, retirementFileIdentity{}, errors.New("retirement JSON proof exceeds the size limit")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != int64(len(data)) || !after.ModTime().Equal(opened.ModTime()) {
		if err == nil {
			err = errors.New("retirement proof file changed while it was read")
		}
		return nil, retirementFileIdentity{}, err
	}
	return data, retirementFileIdentity{Path: path, Bytes: int64(len(data)), SHA256: digestRetirementBytes(data)}, nil
}

func hashStableRetirementFile(path string) (retirementFileIdentity, error) {
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil {
		return retirementFileIdentity{}, err
	}
	if !before.Mode().IsRegular() {
		return retirementFileIdentity{}, errors.New("retirement proof path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return retirementFileIdentity{}, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return retirementFileIdentity{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return retirementFileIdentity{}, errors.New("retirement proof file changed while it was opened")
	}
	hasher := sha256.New()
	bytesRead, readErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return retirementFileIdentity{}, err
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != bytesRead || !after.ModTime().Equal(opened.ModTime()) {
		if err == nil {
			err = errors.New("retirement proof file changed while it was hashed")
		}
		return retirementFileIdentity{}, err
	}
	return retirementFileIdentity{
		Path: path, Bytes: bytesRead, SHA256: hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

func validateRetirementFileIdentity(identity retirementFileIdentity) error {
	if identity.Path == "" || filepath.Clean(identity.Path) != identity.Path || identity.Bytes < 0 || !validRetirementSHA256(identity.SHA256) {
		return errors.New("invalid retirement checkpoint file identity")
	}
	return nil
}

func decodeStrictRetirementJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validRetirementSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func digestRetirementBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func retirementPathWithin(root string, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func syncRetirementDirectories(paths ...string) error {
	seen := make(map[string]struct{}, len(paths))
	var result error
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = errors.Join(result, syncRetirementDirectory(path))
	}
	return result
}

func syncRetirementDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
