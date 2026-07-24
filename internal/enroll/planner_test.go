package enroll

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/storage"
)

func TestPlannerRequiresStableArchivedSessionAndAllGlobalGates(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(path, []byte("stable-session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10_000, 0)
	if err := os.Chtimes(path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	session := codex.Session{ID: "session", RolloutPath: path, Archived: true, UpdatedAt: now.Add(-2 * time.Hour).Unix()}
	input := Input{
		Sessions: []codex.Session{session}, Now: now,
		Policy: Policy{StableFor: time.Hour, BatchSize: 1, ArchivedOnly: true},
		Gates:  Gates{DoctorHealthy: true, MountHealthy: true, CanonicalNamespace: true, NamespaceActive: true, NamespaceReady: true, EnrollmentAllowed: true},
		Budget: allowingBudget{},
	}
	first, err := Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	assertDecisionReason(t, first, "session", ReasonStabilityPending)
	if len(first.Selected) != 0 {
		t.Fatalf("first observation selected a session: %#v", first)
	}

	input.Now = now.Add(2 * time.Hour)
	input.Previous = first.Observations
	second, err := Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Selected) != 1 || second.Selected[0].SessionID != "session" {
		t.Fatalf("stable archived session was not selected: %#v", second)
	}

	for name, test := range map[string]struct {
		mutate func(*Input)
		reason Reason
	}{
		"doctor":    {mutate: func(input *Input) { input.Gates.DoctorHealthy = false }, reason: ReasonDoctorUnhealthy},
		"mount":     {mutate: func(input *Input) { input.Gates.MountHealthy = false }, reason: ReasonMountUnhealthy},
		"namespace": {mutate: func(input *Input) { input.Gates.CanonicalNamespace = false }, reason: ReasonNamespaceDisabled},
		"stage":     {mutate: func(input *Input) { input.Gates.EnrollmentAllowed = false }, reason: ReasonPromotionStage},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := input
			test.mutate(&candidate)
			plan, err := Build(context.Background(), candidate)
			if err != nil {
				t.Fatal(err)
			}
			assertDecisionReason(t, plan, "session", test.reason)
		})
	}
}

func TestPlannerSeparatesActiveChangingManagedWriterBudgetAndBatchCases(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(20_000, 0)
	makeSession := func(id string, archived bool) codex.Session {
		path := filepath.Join(root, id+".jsonl")
		if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		return codex.Session{ID: id, RolloutPath: path, Archived: archived, UpdatedAt: now.Add(-2 * time.Hour).Unix()}
	}
	active := makeSession("active", false)
	changing := makeSession("changing", true)
	managed := makeSession("managed", true)
	writer := makeSession("writer", true)
	firstBatch := makeSession("batch-a", true)
	secondBatch := makeSession("batch-b", true)
	budgeted := makeSession("budgeted", true)
	previous := make(Observations)
	for _, session := range []codex.Session{changing, managed, writer, firstBatch, secondBatch, budgeted} {
		info, err := os.Stat(session.RolloutPath)
		if err != nil {
			t.Fatal(err)
		}
		previous[session.ID] = Observation{Path: session.RolloutPath, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), StableSinceUnixNano: now.Add(-2 * time.Hour).UnixNano()}
	}
	if err := os.WriteFile(changing.RolloutPath, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := Input{
		Sessions: []codex.Session{active, changing, managed, writer, firstBatch, secondBatch, budgeted},
		Managed:  map[string]struct{}{"managed": {}}, Previous: previous, Now: now,
		Policy:       Policy{StableFor: time.Hour, BatchSize: 1, ArchivedOnly: true},
		Gates:        Gates{DoctorHealthy: true, MountHealthy: true, CanonicalNamespace: true, NamespaceActive: true, NamespaceReady: true, EnrollmentAllowed: true},
		WriterActive: func(_ context.Context, session codex.Session) (bool, error) { return session.ID == "writer", nil },
		Budget:       rejectingSessionBudget{sessionID: "budgeted"},
	}
	plan, err := Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	assertDecisionReason(t, plan, "active", ReasonNotArchived)
	assertDecisionReason(t, plan, "changing", ReasonFileChanged)
	assertDecisionReason(t, plan, "managed", ReasonAlreadyManaged)
	assertDecisionReason(t, plan, "writer", ReasonWriterActive)
	assertDecisionReason(t, plan, "batch-b", ReasonBatchLimit)
	assertDecisionReason(t, plan, "budgeted", ReasonInsufficientBudget)
	if len(plan.Selected) != 1 || plan.Selected[0].SessionID != "batch-a" {
		t.Fatalf("bounded selection = %#v", plan.Selected)
	}
}

func TestPlannerSelectsStableWriterFreeActiveSessionWhenPolicyAllowsIt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "active.jsonl")
	if err := os.WriteFile(path, []byte("active\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(25_000, 0)
	if err := os.Chtimes(path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	session := codex.Session{ID: "active", RolloutPath: path, Archived: false, UpdatedAt: now.Add(-2 * time.Hour).Unix()}
	plan, err := Build(context.Background(), Input{
		Sessions: []codex.Session{session}, Now: now,
		Previous:     Observations{"active": {Path: path, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), StableSinceUnixNano: now.Add(-2 * time.Hour).UnixNano()}},
		Policy:       Policy{StableFor: time.Hour, BatchSize: 1, ArchivedOnly: false},
		Gates:        Gates{DoctorHealthy: true, MountHealthy: true, CanonicalNamespace: true, NamespaceActive: true, NamespaceReady: true, EnrollmentAllowed: true},
		WriterActive: func(context.Context, codex.Session) (bool, error) { return false, nil },
		Budget:       allowingBudget{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Selected) != 1 || plan.Selected[0].SessionID != "active" {
		t.Fatalf("stable writer-free active session was not selected: %#v", plan)
	}
}

func TestPlannerDiscoversExistingNewAndForkedSessionsAcrossCycles(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(30_000, 0)
	makeSession := func(id string) codex.Session {
		path := filepath.Join(root, id+".jsonl")
		if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		return codex.Session{ID: id, RolloutPath: path, Archived: true, UpdatedAt: now.Add(-2 * time.Hour).Unix()}
	}
	existing := makeSession("existing")
	input := Input{
		Sessions: []codex.Session{existing}, Now: now,
		Policy: Policy{StableFor: time.Hour, BatchSize: 3, ArchivedOnly: true},
		Gates:  Gates{DoctorHealthy: true, MountHealthy: true, CanonicalNamespace: true, NamespaceActive: true, NamespaceReady: true, EnrollmentAllowed: true},
		Budget: allowingBudget{},
	}
	first, err := Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	newSession := makeSession("new")
	fork := makeSession("fork")
	input.Sessions = []codex.Session{existing, newSession, fork}
	input.Previous = first.Observations
	input.Now = now.Add(2 * time.Hour)
	second, err := Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Selected) != 1 || second.Selected[0].SessionID != "existing" {
		t.Fatalf("existing session was not selected while new discoveries observed: %#v", second)
	}
	assertDecisionReason(t, second, "new", ReasonStabilityPending)
	assertDecisionReason(t, second, "fork", ReasonStabilityPending)

	input.Managed = map[string]struct{}{"existing": {}}
	input.Previous = second.Observations
	input.Now = now.Add(4 * time.Hour)
	third, err := Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Selected) != 2 || third.Selected[0].SessionID != "fork" || third.Selected[1].SessionID != "new" {
		t.Fatalf("new and forked sessions were not selected after becoming stable: %#v", third.Selected)
	}
}

func TestPlannerDefersMissingRoutesWhileCanonicalNamespaceWarms(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-visible-yet.jsonl")
	plan, err := Build(context.Background(), Input{
		Sessions: []codex.Session{{ID: "warming", RolloutPath: missing}},
		Policy:   Policy{StableFor: time.Hour, BatchSize: 1},
		Gates: Gates{
			DoctorHealthy: true, MountHealthy: true, CanonicalNamespace: true,
			NamespaceActive: true, NamespaceReady: false, EnrollmentAllowed: true,
		},
		Budget: allowingBudget{},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecisionReason(t, plan, "warming", ReasonMountWarming)
	for _, reason := range plan.Decisions[0].Reasons {
		if reason == ReasonInvalidPath {
			t.Fatalf("temporarily unavailable route was classified as invalid: %#v", plan.Decisions[0])
		}
	}
}

func TestApplyRevalidatesFingerprintAndSkipsAlreadyManagedSessions(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.jsonl")
	second := filepath.Join(root, "second.jsonl")
	if err := os.WriteFile(first, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstInfo, _ := os.Stat(first)
	secondInfo, _ := os.Stat(second)
	plan := Plan{Selected: []Decision{
		{SessionID: "first", RolloutPath: first, Selected: true, Fingerprint: Fingerprint{Size: firstInfo.Size(), ModTimeUnixNano: firstInfo.ModTime().UnixNano()}},
		{SessionID: "second", RolloutPath: second, Selected: true, Fingerprint: Fingerprint{Size: secondInfo.Size(), ModTimeUnixNano: secondInfo.ModTime().UnixNano()}},
	}}
	if err := os.WriteFile(first, []byte("first changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	applied := make([]string, 0)
	result, err := Apply(context.Background(), plan, ApplyOptions{
		IsManaged: func(_ context.Context, sessionID string) (bool, error) { return sessionID == "second", nil },
		Apply: func(_ context.Context, decision Decision) error {
			applied = append(applied, decision.SessionID)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 || result.Applied != 0 || result.SkippedChanged != 1 || result.SkippedManaged != 1 {
		t.Fatalf("apply revalidation result = %#v applied=%v", result, applied)
	}
}

func TestRevalidateReturnsOnlyStableUnmanagedDecisions(t *testing.T) {
	root := t.TempDir()
	paths := map[string]string{
		"stable":  filepath.Join(root, "stable.jsonl"),
		"changed": filepath.Join(root, "changed.jsonl"),
		"managed": filepath.Join(root, "managed.jsonl"),
	}
	for id, path := range paths {
		if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selected := make([]Decision, 0, len(paths))
	for _, id := range []string{"stable", "changed", "managed"} {
		info, err := os.Stat(paths[id])
		if err != nil {
			t.Fatal(err)
		}
		selected = append(selected, Decision{
			SessionID: id, RolloutPath: paths[id], Selected: true,
			Fingerprint: Fingerprint{Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano()},
		})
	}
	if err := os.WriteFile(paths["changed"], []byte("changed after planning\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	decisions, result, err := Revalidate(context.Background(), Plan{Selected: selected}, RevalidateOptions{
		IsManaged: func(_ context.Context, sessionID string) (bool, error) {
			return sessionID == "managed", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].SessionID != "stable" {
		t.Fatalf("revalidated decisions = %#v", decisions)
	}
	if result.Selected != 3 || result.Applied != 0 || result.SkippedChanged != 1 || result.SkippedManaged != 1 {
		t.Fatalf("revalidation result = %#v", result)
	}
}

func TestRevalidateHonorsBatchLimitBeforeFiltering(t *testing.T) {
	root := t.TempDir()
	selected := make([]Decision, 0, 3)
	for _, id := range []string{"managed", "stable", "outside-limit"} {
		path := filepath.Join(root, id+".jsonl")
		if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		selected = append(selected, Decision{
			SessionID: id, RolloutPath: path, Selected: true,
			Fingerprint: Fingerprint{Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano()},
		})
	}

	decisions, result, err := Revalidate(context.Background(), Plan{Selected: selected}, RevalidateOptions{
		Limit: 2,
		IsManaged: func(_ context.Context, sessionID string) (bool, error) {
			return sessionID == "managed", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].SessionID != "stable" {
		t.Fatalf("limited decisions = %#v", decisions)
	}
	if result.Selected != 2 || result.SkippedManaged != 1 || result.SkippedChanged != 0 {
		t.Fatalf("limited revalidation result = %#v", result)
	}
}

type allowingBudget struct{}

func (allowingBudget) Check(_ context.Context, projection storage.Projection) (storage.Assessment, error) {
	return storage.Assessment{Budget: storage.BudgetReport{Operation: projection.Operation, Allowed: true}}, nil
}

type rejectingSessionBudget struct {
	sessionID string
}

func (b rejectingSessionBudget) Check(_ context.Context, projection storage.Projection) (storage.Assessment, error) {
	if projection.Operation == "enroll:"+b.sessionID {
		return storage.Assessment{}, storage.ErrBudgetExceeded
	}
	return storage.Assessment{Budget: storage.BudgetReport{Operation: projection.Operation, Allowed: true}}, nil
}

func assertDecisionReason(t *testing.T, plan Plan, sessionID string, reason Reason) {
	t.Helper()
	for _, decision := range plan.Decisions {
		if decision.SessionID != sessionID {
			continue
		}
		for _, found := range decision.Reasons {
			if found == reason {
				return
			}
		}
		t.Fatalf("decision %s reasons = %v, want %s", sessionID, decision.Reasons, reason)
	}
	t.Fatalf("decision not found: %s", sessionID)
}
