package storage

import (
	"errors"
	"fmt"
	"math"
)

var ErrBudgetExceeded = errors.New("storage budget exceeded")

type RejectionReason string

const (
	RejectionPhysicalBudget   RejectionReason = "physical-budget"
	RejectionTemporaryBudget  RejectionReason = "temporary-budget"
	RejectionFreeSpaceReserve RejectionReason = "free-space-reserve"
)

type Limits struct {
	MaxPhysicalBytes      int64 `json:"max_physical_bytes"`
	MaxTemporaryBytes     int64 `json:"max_temporary_bytes"`
	FreeSpaceReserveBytes int64 `json:"free_space_reserve_bytes"`
}

type BudgetRequest struct {
	Operation                       string `json:"operation"`
	CurrentPhysicalBytes            int64  `json:"current_physical_bytes"`
	AdditionalPersistentBytes       int64  `json:"additional_persistent_bytes"`
	TemporaryBytes                  int64  `json:"temporary_bytes"`
	TemporaryPersistentOverlapBytes int64  `json:"temporary_persistent_overlap_bytes"`
	ReclaimableBytes                int64  `json:"reclaimable_bytes"`
	AvailableBytes                  int64  `json:"available_bytes"`
}

type BudgetReport struct {
	Operation                 string            `json:"operation"`
	Allowed                   bool              `json:"allowed"`
	CurrentPhysicalBytes      int64             `json:"current_physical_bytes"`
	ProjectedPeakBytes        int64             `json:"projected_peak_bytes"`
	ProjectedFinalBytes       int64             `json:"projected_final_bytes"`
	ProjectedReclaimableBytes int64             `json:"projected_reclaimable_bytes"`
	AvailableBytes            int64             `json:"available_bytes"`
	FreeSpaceAfterPeak        int64             `json:"free_space_after_peak"`
	Rejections                []RejectionReason `json:"rejections,omitempty"`
}

func CheckBudget(request BudgetRequest, limits Limits) (BudgetReport, error) {
	if request.Operation == "" {
		return BudgetReport{}, errors.New("storage budget operation is required")
	}
	if request.CurrentPhysicalBytes < 0 || request.AdditionalPersistentBytes < 0 || request.TemporaryBytes < 0 || request.TemporaryPersistentOverlapBytes < 0 || request.ReclaimableBytes < 0 || request.AvailableBytes < 0 {
		return BudgetReport{}, errors.New("storage budget byte counts cannot be negative")
	}
	if request.TemporaryPersistentOverlapBytes > request.TemporaryBytes || request.TemporaryPersistentOverlapBytes > request.AdditionalPersistentBytes {
		return BudgetReport{}, errors.New("temporary and persistent overlap exceeds the projected bytes")
	}
	if limits.MaxPhysicalBytes < 0 || limits.MaxTemporaryBytes < 0 || limits.FreeSpaceReserveBytes < 0 {
		return BudgetReport{}, errors.New("storage limits cannot be negative")
	}
	additional, overflow := addBudgetBytes(request.AdditionalPersistentBytes, request.TemporaryBytes)
	if overflow {
		return BudgetReport{}, errors.New("storage budget additional byte projection overflow")
	}
	additional -= request.TemporaryPersistentOverlapBytes
	peak, overflow := addBudgetBytes(request.CurrentPhysicalBytes, additional)
	if overflow {
		return BudgetReport{}, errors.New("storage budget peak byte projection overflow")
	}
	beforeReclaim, overflow := addBudgetBytes(request.CurrentPhysicalBytes, request.AdditionalPersistentBytes)
	if overflow {
		return BudgetReport{}, errors.New("storage budget final byte projection overflow")
	}
	final := beforeReclaim - min(request.ReclaimableBytes, beforeReclaim)
	freeAfterPeak := request.AvailableBytes - additional
	report := BudgetReport{
		Operation: request.Operation, Allowed: true,
		CurrentPhysicalBytes: request.CurrentPhysicalBytes,
		ProjectedPeakBytes:   peak, ProjectedFinalBytes: final,
		ProjectedReclaimableBytes: request.ReclaimableBytes,
		AvailableBytes:            request.AvailableBytes, FreeSpaceAfterPeak: freeAfterPeak,
	}
	if limits.MaxPhysicalBytes > 0 && peak > limits.MaxPhysicalBytes {
		report.Rejections = append(report.Rejections, RejectionPhysicalBudget)
	}
	if limits.MaxTemporaryBytes > 0 && request.TemporaryBytes > limits.MaxTemporaryBytes {
		report.Rejections = append(report.Rejections, RejectionTemporaryBudget)
	}
	if freeAfterPeak < limits.FreeSpaceReserveBytes {
		report.Rejections = append(report.Rejections, RejectionFreeSpaceReserve)
	}
	if len(report.Rejections) != 0 {
		report.Allowed = false
		return report, fmt.Errorf("%w: %s rejected by %v", ErrBudgetExceeded, request.Operation, report.Rejections)
	}
	return report, nil
}

func addBudgetBytes(left int64, right int64) (int64, bool) {
	if left > math.MaxInt64-right {
		return 0, true
	}
	return left + right, false
}
