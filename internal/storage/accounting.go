package storage

import "context"

type MutationAccounting struct {
	Before                    Inventory    `json:"before"`
	Budget                    BudgetReport `json:"budget"`
	After                     Inventory    `json:"after"`
	ProjectedReclaimableBytes int64        `json:"projected_reclaimable_bytes"`
	ActualReclaimedBytes      int64        `json:"actual_reclaimed_bytes"`
	AfterInventoryError       string       `json:"after_inventory_error,omitempty"`
}

func CompleteAccounting(ctx context.Context, assessment Assessment, storeDir string) *MutationAccounting {
	accounting := &MutationAccounting{
		Before: assessment.Inventory, Budget: assessment.Budget,
		ProjectedReclaimableBytes: assessment.Budget.ProjectedReclaimableBytes,
	}
	if storeDir == "" {
		return accounting
	}
	after, err := Scan(ctx, Options{StoreDir: storeDir, AllowMetadataIssues: true})
	if err != nil {
		accounting.AfterInventoryError = err.Error()
		return accounting
	}
	accounting.After = after
	if assessment.Inventory.StoreDir != "" && assessment.Inventory.TotalPhysicalBytes > after.TotalPhysicalBytes {
		accounting.ActualReclaimedBytes = assessment.Inventory.TotalPhysicalBytes - after.TotalPhysicalBytes
	}
	return accounting
}
