package enroll

import (
	"encoding/json"
	"math"
	"os"
)

// BatchTuning is what the last cycles measured about their own cost.
//
// A cycle rebuilds the entire pack whether it folds one session or five
// hundred, so that rebuild is a fixed toll paid once per cycle and the only way
// to make it cheap per session is to fold more of them behind it. Folding
// itself is the opposite: it costs what it costs, per session, every time.
type BatchTuning struct {
	// PackSeconds is the fixed toll: the pack rebuild and the verification
	// around it, which grows with the store rather than with the batch.
	PackSeconds float64 `json:"pack_seconds"`
	// FoldSecondsPerSession is the marginal cost of taking one more session.
	FoldSecondsPerSession float64 `json:"fold_seconds_per_session"`
}

// Observe folds one cycle's measurements into the running estimate.
//
// The estimate is smoothed because a single cycle is noisy — a batch of large
// sessions or a busy machine should nudge the target, not redefine it.
func (t BatchTuning) Observe(packSeconds float64, foldSeconds float64, sessions int) BatchTuning {
	updated := t
	if packSeconds > 0 {
		updated.PackSeconds = blend(t.PackSeconds, packSeconds)
	}
	if sessions > 0 && foldSeconds > 0 {
		updated.FoldSecondsPerSession = blend(t.FoldSecondsPerSession, foldSeconds/float64(sessions))
	}
	return updated
}

func blend(previous, sample float64) float64 {
	if previous <= 0 {
		return sample
	}
	return previous*0.7 + sample*0.3
}

// AutoBatchLimits bounds an automatic batch.
type AutoBatchLimits struct {
	Minimum int
	Maximum int
	// OverheadShare is how much of a cycle the fixed toll may take. A quarter
	// means the batch is sized so folding does at least three times the work of
	// the rebuild behind it.
	OverheadShare float64
}

// DefaultAutoBatchLimits is deliberately unadventurous at the top end: a larger
// batch is cheaper per session but holds the store for longer and loses more
// work if the cycle fails partway.
var DefaultAutoBatchLimits = AutoBatchLimits{Minimum: 10, Maximum: 400, OverheadShare: 0.25}

// AutoBatchSize picks how many sessions the next cycle should take.
//
// Until a cycle has measured itself there is nothing to reason from, so the
// minimum applies: it is already far better than one, and the first cycle
// replaces the guess with a measurement.
func AutoBatchSize(tuning BatchTuning, limits AutoBatchLimits) int {
	if limits.Minimum < 1 {
		limits.Minimum = 1
	}
	if limits.Maximum < limits.Minimum {
		limits.Maximum = limits.Minimum
	}
	if limits.OverheadShare <= 0 || limits.OverheadShare >= 1 {
		limits.OverheadShare = DefaultAutoBatchLimits.OverheadShare
	}
	if tuning.PackSeconds <= 0 || tuning.FoldSecondsPerSession <= 0 {
		return limits.Minimum
	}
	// Keep the rebuild under its share of the cycle:
	//   pack <= share * (pack + batch*fold)  =>  batch >= pack*(1-share)/(share*fold)
	needed := tuning.PackSeconds * (1 - limits.OverheadShare) /
		(limits.OverheadShare * tuning.FoldSecondsPerSession)
	if math.IsNaN(needed) || math.IsInf(needed, 0) {
		return limits.Minimum
	}
	batch := int(math.Ceil(needed))
	if batch < limits.Minimum {
		return limits.Minimum
	}
	if batch > limits.Maximum {
		return limits.Maximum
	}
	return batch
}

type tuningFile struct {
	Version               int     `json:"version"`
	PackSeconds           float64 `json:"pack_seconds"`
	FoldSecondsPerSession float64 `json:"fold_seconds_per_session"`
}

const tuningVersion = 1

// LoadTuning reads the cost estimate a previous cycle recorded. A missing or
// unreadable file is not an error: the batch simply falls back to its minimum
// until a cycle measures itself again.
func LoadTuning(path string) BatchTuning {
	data, err := os.ReadFile(path)
	if err != nil {
		return BatchTuning{}
	}
	var stored tuningFile
	if err := json.Unmarshal(data, &stored); err != nil || stored.Version != tuningVersion {
		return BatchTuning{}
	}
	if stored.PackSeconds < 0 || stored.FoldSecondsPerSession < 0 {
		return BatchTuning{}
	}
	return BatchTuning{PackSeconds: stored.PackSeconds, FoldSecondsPerSession: stored.FoldSecondsPerSession}
}

// SaveTuning records the estimate for the next cycle to size itself from.
func SaveTuning(path string, tuning BatchTuning) error {
	return writeAtomicJSON(path, ".tuning-*.tmp", tuningFile{
		Version:               tuningVersion,
		PackSeconds:           tuning.PackSeconds,
		FoldSecondsPerSession: tuning.FoldSecondsPerSession,
	})
}
