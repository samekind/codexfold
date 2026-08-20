package cli

import (
	"testing"

	"github.com/samekind/codexfold/internal/enroll"
)

// A zero selection must explain itself in the text report. Before this, the
// reason was reachable only through --json, which made a blocked enrollment
// look indistinguishable from an idle one.
func TestSummarizeUnselectedReasons(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		decisions []enroll.Decision
		want      string
	}{
		{name: "no decisions", decisions: nil, want: ""},
		{
			name: "selected sessions are not counted",
			decisions: []enroll.Decision{
				{SessionID: "a", Selected: true},
			},
			want: "",
		},
		{
			name: "most frequent reason first",
			decisions: []enroll.Decision{
				{SessionID: "a", Reasons: []enroll.Reason{enroll.ReasonAlreadyManaged}},
				{SessionID: "b", Reasons: []enroll.Reason{enroll.ReasonAlreadyManaged}},
				{SessionID: "c", Reasons: []enroll.Reason{enroll.ReasonInsufficientBudget}},
			},
			want: "already-managed=2 insufficient-storage-budget=1",
		},
		{
			name: "equal counts are ordered by name for a stable report",
			decisions: []enroll.Decision{
				{SessionID: "a", Reasons: []enroll.Reason{enroll.ReasonWriterActive}},
				{SessionID: "b", Reasons: []enroll.Reason{enroll.ReasonMountUnhealthy}},
			},
			want: "mount-unhealthy=1 writer-active=1",
		},
		{
			name: "every reason on one decision is counted",
			decisions: []enroll.Decision{
				{SessionID: "a", Reasons: []enroll.Reason{enroll.ReasonStabilityPending, enroll.ReasonWriterActive}},
			},
			want: "stability-observation-pending=1 writer-active=1",
		},
		{
			name: "a reasonless hold back is reported rather than dropped",
			decisions: []enroll.Decision{
				{SessionID: "a"},
			},
			want: "unknown=1",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := summarizeUnselectedReasons(testCase.decisions); got != testCase.want {
				t.Fatalf("summary = %q, want %q", got, testCase.want)
			}
		})
	}
}
