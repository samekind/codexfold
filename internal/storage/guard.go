package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
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

// DefaultSharedInventoryMaxAge bounds how stale the shared inventory may be
// before a background refresh starts. Free space is still probed on every check.
const DefaultSharedInventoryMaxAge = 2 * time.Minute

// scanInventory is replaced in tests to count or delay scans.
var scanInventory = Scan

// SharedInventoryGuard prices foreground work (copy-on-write, materialization)
// against a process-wide inventory that is refreshed in the background. A full
// Scan walks the whole store and takes seconds, so foreground paths that hold
// handle or session locks must never run one themselves. Only the very first
// check in a process waits for a scan; Prime starts it ahead of time.
type SharedInventoryGuard struct {
	Guard  Guard
	MaxAge time.Duration
}

type inventoryCache struct {
	mu       sync.Mutex
	inv      Inventory
	loadedAt time.Time
	loaded   bool
	flight   chan struct{}
	lastErr  error
}

var (
	sharedInventoriesMu sync.Mutex
	sharedInventories   = map[string]*inventoryCache{}
)

func sharedInventoryFor(store string) *inventoryCache {
	sharedInventoriesMu.Lock()
	defer sharedInventoriesMu.Unlock()
	cache := sharedInventories[store]
	if cache == nil {
		cache = &inventoryCache{}
		sharedInventories[store] = cache
	}
	return cache
}

func (c *inventoryCache) startRefreshLocked(store string) {
	flight := make(chan struct{})
	c.flight = flight
	scan := scanInventory
	go func() {
		inventory, err := scan(context.Background(), Options{StoreDir: store, AllowMetadataIssues: true})
		c.mu.Lock()
		if err == nil {
			c.inv, c.loaded, c.loadedAt, c.lastErr = inventory, true, time.Now(), nil
		} else {
			c.lastErr = err
		}
		c.flight = nil
		c.mu.Unlock()
		close(flight)
	}()
}

func (c *inventoryCache) get(ctx context.Context, store string, maxAge time.Duration) (Inventory, error) {
	c.mu.Lock()
	if c.loaded {
		inventory := c.inv
		if c.flight == nil && time.Since(c.loadedAt) > maxAge {
			c.startRefreshLocked(store)
		}
		c.mu.Unlock()
		return inventory, nil
	}
	if c.flight == nil {
		c.startRefreshLocked(store)
	}
	flight := c.flight
	c.mu.Unlock()
	select {
	case <-flight:
	case <-ctx.Done():
		return Inventory{}, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.inv, nil
	}
	return Inventory{}, c.lastErr
}

// Prime starts the first inventory scan in the background without waiting.
func (g SharedInventoryGuard) Prime() {
	store, err := g.Guard.absoluteStore()
	if err != nil {
		return
	}
	PrepareReadReserve(store)
	cache := sharedInventoryFor(store)
	cache.mu.Lock()
	if !cache.loaded && cache.flight == nil {
		cache.startRefreshLocked(store)
	}
	cache.mu.Unlock()
}

func (g SharedInventoryGuard) Check(ctx context.Context, projection Projection) (Assessment, error) {
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	store, err := g.Guard.absoluteStore()
	if err != nil {
		return Assessment{}, err
	}
	maxAge := g.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultSharedInventoryMaxAge
	}
	inventory, err := sharedInventoryFor(store).get(ctx, store, maxAge)
	if err != nil {
		return Assessment{}, err
	}
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
