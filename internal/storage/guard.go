package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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

// OnceInventoryGuard prices a bounded planning batch against one store
// snapshot while still probing free space for every decision. Mutating
// operations must continue to use a fresh Guard.Check at their own fence.
type OnceInventoryGuard struct {
	Guard     Guard
	mu        sync.Mutex
	inventory Inventory
	loaded    bool
}

func (g *OnceInventoryGuard) Check(ctx context.Context, projection Projection) (Assessment, error) {
	if g == nil {
		return Assessment{}, errors.New("storage inventory guard is required")
	}
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	store, err := g.Guard.absoluteStore()
	if err != nil {
		return Assessment{}, err
	}
	g.mu.Lock()
	if !g.loaded {
		inventory, scanErr := Scan(ctx, Options{StoreDir: store, AllowMetadataIssues: true})
		if scanErr != nil {
			g.mu.Unlock()
			return Assessment{}, scanErr
		}
		g.inventory = inventory
		g.loaded = true
	}
	inventory := g.inventory
	g.mu.Unlock()
	return g.Guard.checkWithInventory(ctx, store, inventory, projection)
}

func (g Guard) Check(ctx context.Context, projection Projection) (Assessment, error) {
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	store, err := g.absoluteStore()
	if err != nil {
		return Assessment{}, err
	}
	inventory, err := Scan(ctx, Options{StoreDir: store, AllowMetadataIssues: true})
	if err != nil {
		return Assessment{}, err
	}
	return g.checkWithInventory(ctx, store, inventory, projection)
}

func (g Guard) absoluteStore() (string, error) {
	if g.StoreDir == "" {
		return "", errors.New("storage guard store directory is required")
	}
	store, err := filepath.Abs(g.StoreDir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(store), nil
}

func (g Guard) checkWithInventory(ctx context.Context, store string, inventory Inventory, projection Projection) (Assessment, error) {
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	if filepath.Clean(inventory.StoreDir) != store {
		return Assessment{}, errors.New("storage inventory does not match guard store")
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
