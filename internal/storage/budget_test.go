package storage

import (
	"errors"
	"testing"
)

func TestCheckBudgetCalculatesPeakWithoutSubtractingFutureReclamation(t *testing.T) {
	report, err := CheckBudget(BudgetRequest{
		Operation:                 "compact",
		CurrentPhysicalBytes:      100,
		AdditionalPersistentBytes: 30,
		TemporaryBytes:            80,
		ReclaimableBytes:          70,
		AvailableBytes:            1_000,
	}, Limits{
		MaxPhysicalBytes:      500,
		MaxTemporaryBytes:     100,
		FreeSpaceReserveBytes: 200,
	})
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if !report.Allowed || report.ProjectedPeakBytes != 210 || report.ProjectedFinalBytes != 60 || report.ProjectedReclaimableBytes != 70 {
		t.Fatalf("unexpected report: %#v", report)
	}
	if report.FreeSpaceAfterPeak != 890 {
		t.Fatalf("free space after peak = %d, want 890", report.FreeSpaceAfterPeak)
	}
}

func TestCheckBudgetDoesNotDoubleCountTemporaryBytesThatBecomePersistent(t *testing.T) {
	report, err := CheckBudget(BudgetRequest{
		Operation:                       "copy-on-write",
		CurrentPhysicalBytes:            100,
		AdditionalPersistentBytes:       80,
		TemporaryBytes:                  80,
		TemporaryPersistentOverlapBytes: 80,
		AvailableBytes:                  1_000,
	}, Limits{})
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if report.ProjectedPeakBytes != 180 || report.ProjectedFinalBytes != 180 || report.FreeSpaceAfterPeak != 920 {
		t.Fatalf("temporary rename projection = %#v", report)
	}
}

func TestCheckBudgetRejectsEveryHardLimit(t *testing.T) {
	tests := []struct {
		name    string
		request BudgetRequest
		limits  Limits
		reason  RejectionReason
	}{
		{
			name: "physical footprint",
			request: BudgetRequest{
				Operation: "pack", CurrentPhysicalBytes: 90, AdditionalPersistentBytes: 20,
				AvailableBytes: 1_000,
			},
			limits: Limits{MaxPhysicalBytes: 100},
			reason: RejectionPhysicalBudget,
		},
		{
			name: "temporary bytes",
			request: BudgetRequest{
				Operation: "rollback", CurrentPhysicalBytes: 20, TemporaryBytes: 81,
				AvailableBytes: 1_000,
			},
			limits: Limits{MaxTemporaryBytes: 80},
			reason: RejectionTemporaryBudget,
		},
		{
			name: "free space reserve",
			request: BudgetRequest{
				Operation: "migrate", CurrentPhysicalBytes: 20, AdditionalPersistentBytes: 30, TemporaryBytes: 40,
				AvailableBytes: 100,
			},
			limits: Limits{FreeSpaceReserveBytes: 31},
			reason: RejectionFreeSpaceReserve,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := CheckBudget(test.request, test.limits)
			if !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("error = %v, want ErrBudgetExceeded", err)
			}
			if report.Allowed || len(report.Rejections) != 1 || report.Rejections[0] != test.reason {
				t.Fatalf("unexpected rejection report: %#v", report)
			}
		})
	}
}

func TestCheckBudgetRejectsInvalidOrOverflowingProjections(t *testing.T) {
	if _, err := CheckBudget(BudgetRequest{Operation: "fold", CurrentPhysicalBytes: -1}, Limits{}); err == nil {
		t.Fatal("negative current bytes should fail")
	}
	if _, err := CheckBudget(BudgetRequest{Operation: "fold", CurrentPhysicalBytes: int64(^uint64(0) >> 1), TemporaryBytes: 1, AvailableBytes: 10}, Limits{}); err == nil {
		t.Fatal("overflowing peak projection should fail")
	}
}
