package tray

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
)

type spaceAccounting struct {
	Logical, Physical, CompressionLogical, Compressed, Pending int64
	UpdatedAt                                                  time.Time
}

type accountingCache struct {
	sync.Mutex
	busy  bool
	next  time.Time
	value *spaceAccounting
	err   error
}

func (m *Monitor) RequestAccountingRefresh() {
	m.space.Lock()
	defer m.space.Unlock()
	if !m.space.busy {
		m.space.next = time.Time{}
	}
}

func readSpaceAccounting(ctx context.Context, store string) (spaceAccounting, error) {
	generation, err := pack.CurrentGeneration(store)
	if err != nil {
		return spaceAccounting{}, err
	}
	inventory, err := storage.Scan(ctx, storage.Options{StoreDir: store, AllowMetadataIssues: true})
	if err != nil {
		return spaceAccounting{}, err
	}
	if inventory.IssueCount != 0 {
		return spaceAccounting{}, errors.New("space inventory has unresolved metadata issues")
	}
	logical, err := pack.PublishedLogicalBytes(ctx, store, generation)
	if err != nil {
		return spaceAccounting{}, err
	}
	// Scan the published generation separately: reused pack parts can be hard
	// linked from an older generation, and whole-store category attribution
	// depends on traversal order. Compression cost must include the full pack.
	core, err := storage.Scan(ctx, storage.Options{StoreDir: filepath.Join(store, "packs", generation)})
	if err != nil {
		return spaceAccounting{}, err
	}
	current, err := pack.CurrentGeneration(store)
	if err != nil || current != generation {
		return spaceAccounting{}, errors.New("pack changed during space accounting")
	}
	// These retained representations are separate from current pack storage.
	// Their removal remains subject to the worker's normal verification gates.
	pending := max(int64(0), inventory.TotalPhysicalBytes-core.TotalPhysicalBytes-inventory.Metadata.PhysicalBytes-inventory.ActiveDeltas.PhysicalBytes-inventory.WritableBackings.PhysicalBytes)
	return spaceAccounting{Logical: inventory.LogicalSessionBytes, Physical: inventory.TotalPhysicalBytes,
		CompressionLogical: logical, Compressed: core.TotalPhysicalBytes, Pending: pending, UpdatedAt: time.Now().UTC()}, nil
}

// File enumeration is off the UI/poll thread, bounded to one scan at a time.
// The tray reads metadata only; it never repairs, retires or deletes storage.
func (m *Monitor) accounting(now time.Time) (*spaceAccounting, error) {
	cache := &m.space
	cache.Lock()
	defer cache.Unlock()
	if !cache.busy && !now.Before(cache.next) {
		cache.busy = true
		cache.next = now.Add(time.Minute)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			value, err := readSpaceAccounting(ctx, m.Store)
			cache.Lock()
			defer cache.Unlock()
			cache.busy = false
			cache.err = err
			if err == nil {
				cache.value = &value
			}
		}()
	}
	if cache.value != nil && now.Sub(cache.value.UpdatedAt) <= 5*time.Minute {
		copy := *cache.value
		return &copy, cache.err
	}
	return nil, cache.err
}
