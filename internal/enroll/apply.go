package enroll

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type ApplyOptions struct {
	Limit     int
	IsManaged func(context.Context, string) (bool, error)
	Apply     func(context.Context, Decision) error
}

type ApplyResult struct {
	Selected       int `json:"selected"`
	Applied        int `json:"applied"`
	SkippedChanged int `json:"skipped_changed"`
	SkippedManaged int `json:"skipped_managed"`
}

func Apply(ctx context.Context, plan Plan, options ApplyOptions) (ApplyResult, error) {
	if options.IsManaged == nil || options.Apply == nil {
		return ApplyResult{}, errors.New("enrollment managed-state and apply callbacks are required")
	}
	limit := options.Limit
	if limit <= 0 || limit > len(plan.Selected) {
		limit = len(plan.Selected)
	}
	result := ApplyResult{Selected: min(limit, len(plan.Selected))}
	for _, decision := range plan.Selected[:limit] {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		managed, err := options.IsManaged(ctx, decision.SessionID)
		if err != nil {
			return result, err
		}
		if managed {
			result.SkippedManaged++
			continue
		}
		info, err := os.Lstat(decision.RolloutPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() != decision.Fingerprint.Size || info.ModTime().UnixNano() != decision.Fingerprint.ModTimeUnixNano {
			result.SkippedChanged++
			continue
		}
		if err := options.Apply(ctx, decision); err != nil {
			return result, fmt.Errorf("apply enrollment for %s: %w", decision.SessionID, err)
		}
		result.Applied++
	}
	return result, nil
}
