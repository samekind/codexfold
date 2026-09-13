package enroll

import "testing"

func TestAutoBatchSizeFallsBackToTheMinimumUntilACycleHasMeasuredItself(t *testing.T) {
	limits := DefaultAutoBatchLimits
	for _, tuning := range []BatchTuning{
		{},
		{PackSeconds: 60},
		{FoldSecondsPerSession: 25},
		{PackSeconds: -1, FoldSecondsPerSession: 25},
	} {
		if got := AutoBatchSize(tuning, limits); got != limits.Minimum {
			t.Fatalf("AutoBatchSize(%+v) = %d, want the minimum %d", tuning, got, limits.Minimum)
		}
	}
}

func TestAutoBatchSizeGrowsWithTheRebuildItHasToAmortize(t *testing.T) {
	limits := DefaultAutoBatchLimits
	small := AutoBatchSize(BatchTuning{PackSeconds: 60, FoldSecondsPerSession: 25}, limits)
	large := AutoBatchSize(BatchTuning{PackSeconds: 600, FoldSecondsPerSession: 25}, limits)
	if large <= small {
		t.Fatalf("a costlier rebuild must be spread over more sessions: small=%d large=%d", small, large)
	}
	// A quarter-share target means folding does three times the rebuild's work.
	for _, c := range []struct {
		pack, fold float64
		want       int
	}{
		{pack: 600, fold: 25, want: 72},
		{pack: 1200, fold: 25, want: 144},
		{pack: 600, fold: 50, want: 36},
	} {
		if got := AutoBatchSize(BatchTuning{PackSeconds: c.pack, FoldSecondsPerSession: c.fold}, limits); got != c.want {
			t.Fatalf("AutoBatchSize(pack=%v fold=%v) = %d, want %d", c.pack, c.fold, got, c.want)
		}
	}
}

func TestAutoBatchSizeStaysInsideItsLimits(t *testing.T) {
	limits := AutoBatchLimits{Minimum: 10, Maximum: 40, OverheadShare: 0.25}
	// A rebuild this expensive would ask for thousands; a cycle that long holds
	// the store too long and loses too much when it fails partway.
	if got := AutoBatchSize(BatchTuning{PackSeconds: 100_000, FoldSecondsPerSession: 1}, limits); got != limits.Maximum {
		t.Fatalf("got %d, want the maximum %d", got, limits.Maximum)
	}
	if got := AutoBatchSize(BatchTuning{PackSeconds: 1, FoldSecondsPerSession: 10_000}, limits); got != limits.Minimum {
		t.Fatalf("got %d, want the minimum %d", got, limits.Minimum)
	}
}

func TestBatchTuningSmoothsAwayASingleNoisyCycle(t *testing.T) {
	settled := BatchTuning{PackSeconds: 100, FoldSecondsPerSession: 20}
	// One cycle of unusually large sessions should nudge the estimate, not
	// replace it.
	spiked := settled.Observe(1000, 2000, 10)
	if spiked.PackSeconds <= settled.PackSeconds || spiked.PackSeconds >= 1000 {
		t.Fatalf("pack estimate should move partway, got %v", spiked.PackSeconds)
	}
	if spiked.FoldSecondsPerSession <= settled.FoldSecondsPerSession || spiked.FoldSecondsPerSession >= 200 {
		t.Fatalf("fold estimate should move partway, got %v", spiked.FoldSecondsPerSession)
	}
	// The first measurement has nothing to blend with and is taken whole.
	first := BatchTuning{}.Observe(80, 500, 20)
	if first.PackSeconds != 80 || first.FoldSecondsPerSession != 25 {
		t.Fatalf("first observation = %+v", first)
	}
}

func TestBatchTuningIgnoresUnusableMeasurements(t *testing.T) {
	settled := BatchTuning{PackSeconds: 100, FoldSecondsPerSession: 20}
	if got := settled.Observe(0, 0, 0); got != settled {
		t.Fatalf("a cycle that measured nothing must not move the estimate: %+v", got)
	}
	if got := settled.Observe(0, 400, 0); got != settled {
		t.Fatalf("folding no sessions says nothing about per-session cost: %+v", got)
	}
}
