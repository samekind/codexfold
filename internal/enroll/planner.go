package enroll

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/storage"
)

type Reason string

const (
	ReasonAlreadyManaged     Reason = "already-managed"
	ReasonNotArchived        Reason = "not-archived"
	ReasonInvalidPath        Reason = "invalid-rollout-path"
	ReasonNamespaceInactive  Reason = "canonical-namespace-inactive"
	ReasonMountWarming       Reason = "mount-warming"
	ReasonStabilityPending   Reason = "stability-observation-pending"
	ReasonFileChanged        Reason = "rollout-changed"
	ReasonWriterActive       Reason = "writer-active"
	ReasonDoctorUnhealthy    Reason = "doctor-unhealthy"
	ReasonMountUnhealthy     Reason = "mount-unhealthy"
	ReasonNamespaceDisabled  Reason = "canonical-namespace-disabled"
	ReasonPromotionStage     Reason = "promotion-stage-blocked"
	ReasonInsufficientBudget Reason = "insufficient-storage-budget"
	ReasonBatchLimit         Reason = "batch-limit"
)

type Policy struct {
	StableFor    time.Duration `json:"stable_for"`
	BatchSize    int           `json:"batch_size"`
	ArchivedOnly bool          `json:"archived_only"`
}

type Gates struct {
	DoctorHealthy      bool `json:"doctor_healthy"`
	MountHealthy       bool `json:"mount_healthy"`
	CanonicalNamespace bool `json:"canonical_namespace"`
	NamespaceActive    bool `json:"namespace_active"`
	NamespaceReady     bool `json:"namespace_ready"`
	EnrollmentAllowed  bool `json:"enrollment_allowed"`
}

type Observation struct {
	Path                string `json:"path"`
	Size                int64  `json:"size"`
	ModTimeUnixNano     int64  `json:"mod_time_unix_nano"`
	StableSinceUnixNano int64  `json:"stable_since_unix_nano"`
}

type Observations map[string]Observation

type Fingerprint struct {
	Size            int64 `json:"size"`
	ModTimeUnixNano int64 `json:"mod_time_unix_nano"`
}

type WriterProbe func(context.Context, codex.Session) (bool, error)

type Input struct {
	Sessions     []codex.Session
	Managed      map[string]struct{}
	Previous     Observations
	Now          time.Time
	Policy       Policy
	Gates        Gates
	WriterActive WriterProbe
	Budget       storage.Checker
}

type Decision struct {
	SessionID   string      `json:"session_id"`
	RolloutPath string      `json:"rollout_path"`
	Archived    bool        `json:"archived"`
	Eligible    bool        `json:"eligible"`
	Selected    bool        `json:"selected"`
	Reasons     []Reason    `json:"reasons,omitempty"`
	Fingerprint Fingerprint `json:"fingerprint"`
}

type Plan struct {
	GeneratedAt  string       `json:"generated_at"`
	Decisions    []Decision   `json:"decisions"`
	Selected     []Decision   `json:"selected"`
	Observations Observations `json:"observations"`
}

func Build(ctx context.Context, input Input) (Plan, error) {
	if input.Now.IsZero() {
		input.Now = time.Now()
	}
	if input.Policy.StableFor <= 0 {
		input.Policy.StableFor = time.Hour
	}
	if input.Policy.BatchSize <= 0 {
		input.Policy.BatchSize = 1
	}
	if input.Previous == nil {
		input.Previous = make(Observations)
	}
	plan := Plan{GeneratedAt: input.Now.UTC().Format(time.RFC3339Nano), Observations: make(Observations)}
	sessions := append([]codex.Session(nil), input.Sessions...)
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].UpdatedAt == sessions[j].UpdatedAt {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].UpdatedAt < sessions[j].UpdatedAt
	})
	var selectedPersistentBytes int64
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		decision := Decision{SessionID: session.ID, RolloutPath: filepath.Clean(session.RolloutPath), Archived: session.Archived}
		if _, managed := input.Managed[session.ID]; managed {
			decision.Reasons = append(decision.Reasons, ReasonAlreadyManaged)
			plan.Decisions = append(plan.Decisions, decision)
			continue
		}
		if !safeSessionID(session.ID) || !filepath.IsAbs(session.RolloutPath) {
			decision.Reasons = append(decision.Reasons, ReasonInvalidPath)
			plan.Decisions = append(plan.Decisions, decision)
			continue
		}
		// A freshly restarted filesystem can report a healthy mount before its
		// native namespace has republished every unmanaged path. Do not turn that
		// transient state into a per-session invalid-path verdict.
		if !input.Gates.MountHealthy {
			decision.Reasons = append(decision.Reasons, ReasonMountUnhealthy)
			plan.Decisions = append(plan.Decisions, decision)
			continue
		}
		if !input.Gates.CanonicalNamespace {
			decision.Reasons = append(decision.Reasons, ReasonNamespaceDisabled)
			plan.Decisions = append(plan.Decisions, decision)
			continue
		}
		if !input.Gates.NamespaceActive {
			decision.Reasons = append(decision.Reasons, ReasonNamespaceInactive)
			plan.Decisions = append(plan.Decisions, decision)
			continue
		}
		if !input.Gates.NamespaceReady {
			decision.Reasons = append(decision.Reasons, ReasonMountWarming)
			plan.Decisions = append(plan.Decisions, decision)
			continue
		}
		info, err := os.Lstat(session.RolloutPath)
		if err != nil || !info.Mode().IsRegular() {
			decision.Reasons = append(decision.Reasons, ReasonInvalidPath)
			plan.Decisions = append(plan.Decisions, decision)
			continue
		}
		decision.Fingerprint = Fingerprint{Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano()}
		previous, observed := input.Previous[session.ID]
		unchanged := observed && filepath.Clean(previous.Path) == decision.RolloutPath && previous.Size == info.Size() && previous.ModTimeUnixNano == info.ModTime().UnixNano()
		observation := Observation{Path: decision.RolloutPath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), StableSinceUnixNano: input.Now.UnixNano()}
		if unchanged {
			observation.StableSinceUnixNano = previous.StableSinceUnixNano
		}
		plan.Observations[session.ID] = observation

		if input.Policy.ArchivedOnly && !session.Archived {
			decision.Reasons = append(decision.Reasons, ReasonNotArchived)
		}
		if !input.Gates.DoctorHealthy {
			decision.Reasons = append(decision.Reasons, ReasonDoctorUnhealthy)
		}
		if !input.Gates.EnrollmentAllowed {
			decision.Reasons = append(decision.Reasons, ReasonPromotionStage)
		}
		if input.WriterActive != nil {
			active, err := input.WriterActive(ctx, session)
			if err != nil {
				return Plan{}, fmt.Errorf("probe writer for %s: %w", session.ID, err)
			}
			if active {
				decision.Reasons = append(decision.Reasons, ReasonWriterActive)
			}
		}
		switch {
		case !observed:
			decision.Reasons = append(decision.Reasons, ReasonStabilityPending)
		case !unchanged:
			decision.Reasons = append(decision.Reasons, ReasonFileChanged)
		case input.Now.Sub(time.Unix(0, observation.StableSinceUnixNano)) < input.Policy.StableFor:
			decision.Reasons = append(decision.Reasons, ReasonStabilityPending)
		case input.Now.Sub(info.ModTime()) < input.Policy.StableFor:
			decision.Reasons = append(decision.Reasons, ReasonStabilityPending)
		}
		if len(decision.Reasons) != 0 {
			plan.Decisions = append(plan.Decisions, decision)
			continue
		}
		projectedPersistent, err := enrollmentPersistentBytes(info.Size())
		if err != nil {
			return Plan{}, err
		}
		cumulative, overflow := addBytes(selectedPersistentBytes, projectedPersistent)
		if overflow {
			return Plan{}, errors.New("enrollment batch byte estimate overflow")
		}
		// Pricing a session against the store costs a full inventory scan, so it
		// is the last question asked rather than the first. A decision that is
		// already made does not change with a price, and once the batch is full
		// nothing more can be selected this cycle: asking anyway meant one scan
		// per candidate, which on a real corpus turned a sub-second plan into one
		// that never finished. Sessions past the limit are priced on the cycle
		// that can actually take them.
		switch {
		case len(decision.Reasons) != 0:
		case len(plan.Selected) >= input.Policy.BatchSize:
			decision.Reasons = append(decision.Reasons, ReasonBatchLimit)
		case input.Budget == nil:
			decision.Reasons = append(decision.Reasons, ReasonInsufficientBudget)
		default:
			if _, err := input.Budget.Check(ctx, storage.Projection{
				Operation: "enroll:" + session.ID, AdditionalPersistentBytes: cumulative, TemporaryBytes: info.Size(),
			}); err != nil {
				if !errors.Is(err, storage.ErrBudgetExceeded) {
					return Plan{}, err
				}
				decision.Reasons = append(decision.Reasons, ReasonInsufficientBudget)
			}
		}
		if len(decision.Reasons) == 0 {
			decision.Eligible = true
			decision.Selected = true
			selectedPersistentBytes = cumulative
			plan.Selected = append(plan.Selected, decision)
		}
		plan.Decisions = append(plan.Decisions, decision)
	}
	return plan, nil
}

// WaitingCount is the Host progress denominator: sessions that are ready now
// or only waiting on idle time / this cycle's one-session limit. Busy, open
// (when archived-only), unhealthy, or already managed sessions are excluded.
func WaitingCount(plan Plan) int {
	count := 0
	for _, decision := range plan.Decisions {
		if decisionCountsAsWaiting(decision) {
			count++
		}
	}
	return count
}

func decisionCountsAsWaiting(decision Decision) bool {
	if decision.Selected || decision.Eligible {
		return true
	}
	if len(decision.Reasons) == 0 {
		return false
	}
	for _, reason := range decision.Reasons {
		switch reason {
		case ReasonStabilityPending, ReasonFileChanged, ReasonBatchLimit:
		default:
			return false
		}
	}
	return true
}

func safeSessionID(sessionID string) bool {
	return sessionID != "" && sessionID != "." && sessionID != ".." && !strings.ContainsAny(sessionID, "/\\\x00")
}

func enrollmentPersistentBytes(rawBytes int64) (int64, error) {
	if rawBytes < 0 {
		return 0, errors.New("enrollment byte estimate cannot be negative")
	}
	overhead := rawBytes/16 + 1<<20
	if rawBytes > math.MaxInt64-overhead {
		return 0, errors.New("enrollment byte estimate overflow")
	}
	estimated := rawBytes + overhead
	if estimated > math.MaxInt64/3 {
		return 0, errors.New("enrollment byte estimate overflow")
	}
	return estimated * 3, nil
}

func addBytes(left int64, right int64) (int64, bool) {
	if right > math.MaxInt64-left {
		return 0, true
	}
	return left + right, false
}
