package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

type Projection struct {
	Operation                       string `json:"operation"`
	AdditionalPersistentBytes       int64  `json:"additional_persistent_bytes"`
	TemporaryBytes                  int64  `json:"temporary_bytes"`
	TemporaryPersistentOverlapBytes int64  `json:"temporary_persistent_overlap_bytes"`
	ReclaimableBytes                int64  `json:"reclaimable_bytes"`
}

type SpaceProbe func(path string) (int64, error)

type Guard struct {
	StoreDir string
	Limits   Limits
	Probe    SpaceProbe
}

type VolumeGuard struct {
	Path   string
	Limits Limits
	Probe  SpaceProbe
}

type Assessment struct {
	Inventory Inventory    `json:"inventory"`
	Budget    BudgetReport `json:"budget"`
}

func (g Guard) Check(ctx context.Context, projection Projection) (Assessment, error) {
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	if g.StoreDir == "" {
		return Assessment{}, errors.New("storage guard store directory is required")
	}
	store, err := filepath.Abs(g.StoreDir)
	if err != nil {
		return Assessment{}, err
	}
	store = filepath.Clean(store)
	inventory, err := Scan(ctx, Options{StoreDir: store, AllowMetadataIssues: true})
	if err != nil {
		return Assessment{}, err
	}
	probe := g.Probe
	if probe == nil {
		probe = AvailableBytes
	}
	available, err := probe(store)
	if err != nil {
		return Assessment{Inventory: inventory}, fmt.Errorf("probe available storage bytes: %w", err)
	}
	report, err := CheckBudget(BudgetRequest{
		Operation:                       projection.Operation,
		CurrentPhysicalBytes:            inventory.TotalPhysicalBytes,
		AdditionalPersistentBytes:       projection.AdditionalPersistentBytes,
		TemporaryBytes:                  projection.TemporaryBytes,
		TemporaryPersistentOverlapBytes: projection.TemporaryPersistentOverlapBytes,
		ReclaimableBytes:                projection.ReclaimableBytes,
		AvailableBytes:                  available,
	}, g.Limits)
	return Assessment{Inventory: inventory, Budget: report}, err
}

func (g VolumeGuard) Check(ctx context.Context, projection Projection) (Assessment, error) {
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	if g.Path == "" {
		return Assessment{}, errors.New("volume guard path is required")
	}
	path := filepath.Clean(g.Path)
	probe := g.Probe
	if probe == nil {
		probe = AvailableBytes
	}
	available, err := probe(path)
	if err != nil {
		return Assessment{}, fmt.Errorf("probe available storage bytes: %w", err)
	}
	limits := g.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits
	}
	report, err := CheckBudget(BudgetRequest{
		Operation: projection.Operation, AdditionalPersistentBytes: projection.AdditionalPersistentBytes,
		TemporaryBytes: projection.TemporaryBytes, TemporaryPersistentOverlapBytes: projection.TemporaryPersistentOverlapBytes,
		ReclaimableBytes: projection.ReclaimableBytes, AvailableBytes: available,
	}, limits)
	return Assessment{Budget: report}, err
}
