package service

import (
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/fsctl"
)

// Grill lock 5.5: while Codex runs, CodexFold is never updated. Preparation,
// checking and installation are permitted only once Desktop, CLI and app-server
// are all gone. An approved promotion must not override that.
func TestEvaluateUpdateRefusesWhileCodexRuns(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		input UpdateInput
	}{
		{
			name: "promoted preview",
			input: UpdateInput{
				Capability: fsctl.FSEnginePreview, DoctorHealthy: true,
				ExplicitPromotion: true, CodexRunning: true,
			},
		},
		{
			name: "production readiness",
			input: UpdateInput{
				Capability: fsctl.Capability("production-ready:macos"), DoctorHealthy: true,
				CodexRunning: true,
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			decision := EvaluateUpdate(testCase.input)
			if decision.Allowed {
				t.Fatal("an update was allowed while Codex was running")
			}
			if !strings.Contains(decision.Reason, "Codex") {
				t.Fatalf("reason = %q, want it to name Codex", decision.Reason)
			}
		})
	}
}

// The guard cannot be satisfied by failing to look. An unknown activity state is
// refused with the same force as an observed running process.
func TestEvaluateUpdateRefusesWhenCodexActivityIsUnknown(t *testing.T) {
	decision := EvaluateUpdate(UpdateInput{
		Capability: fsctl.Capability("production-ready:macos"), DoctorHealthy: true,
		CodexActivityUnknown: true,
	})
	if decision.Allowed {
		t.Fatal("an update was allowed without proving Codex was closed")
	}
	if !strings.Contains(decision.Reason, "Codex") {
		t.Fatalf("reason = %q, want it to name Codex", decision.Reason)
	}
}

// With Codex closed the existing decisions are unchanged, so the guard adds a
// condition rather than replacing the readiness rules.
func TestEvaluateUpdateKeepsExistingDecisionsWhenCodexIsClosed(t *testing.T) {
	if decision := EvaluateUpdate(UpdateInput{Capability: fsctl.FSEnginePreview, DoctorHealthy: true, ExplicitPromotion: true}); !decision.Allowed {
		t.Fatalf("promoted preview with Codex closed = %#v, want allowed", decision)
	}
	if decision := EvaluateUpdate(UpdateInput{Capability: fsctl.FSEnginePreview, DoctorHealthy: true}); decision.Allowed {
		t.Fatal("unpromoted preview must stay blocked")
	}
	if decision := EvaluateUpdate(UpdateInput{Capability: fsctl.FSEnginePreview}); decision.Allowed {
		t.Fatal("an unhealthy doctor must stay blocked")
	}
}
