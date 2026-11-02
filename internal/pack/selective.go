package pack

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

// A build links every previous pack file into the new generation, except the
// files it rewrites. Rewriting copies a file's live blocks, so it costs their
// size up front and frees the file's dead blocks once the old generation is
// collected. Pricing files one at a time lets a nearly full disk compact a
// few files per build instead of needing room for the whole pack at once.

type packUsage struct {
	size int64
	live int64
	// raw is live bytes stored raw, which a rewrite recompresses.
	raw int64
}

type rewritePlan struct {
	rewrite   map[string]bool
	copyBytes int64
	full      bool
}

const (
	// A rewrite is worth it when it frees at least this share of the file.
	rewriteBenefitShare = 10
	// Share of raw block bytes a rewrite is expected to save by compressing
	// them; measured at 30% on a real pack.
	rawRecompressionShare = 30
	rewriteSafetyMargin   = int64(512 << 20)
)

// packFileUsage attributes the live blocks of the next generation to the
// previous pack files that hold them.
func packFileUsage(ctx context.Context, store string, refs *sql.DB, previous *Resolver, blockBytes int64) (map[string]*packUsage, error) {
	sizes, err := resolverPackSizes(previous.packs)
	if err != nil {
		return nil, err
	}
	usage := make(map[string]*packUsage, len(sizes))
	for name, size := range sizes {
		usage[name] = &packUsage{size: size}
	}
	rows, err := refs.QueryContext(ctx, `select digest,raw_bytes from pack_references order by digest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := fold.NewObjectStore(store)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var digest string
		var size int64
		if err := rows.Scan(&digest, &size); err != nil {
			return nil, err
		}
		// Build re-encodes a loose object instead of reusing its packed copy.
		if _, err := os.Lstat(objects.ObjectPath(digest)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		object, found, err := previous.lookupObject(digest)
		if err != nil {
			return nil, err
		}
		if !found || object.RawBytes != size {
			continue
		}
		for _, block := range object.Blocks {
			if block.RawBytes > blockBytes {
				continue
			}
			file := usage[block.Pack]
			if file == nil {
				continue
			}
			file.live += block.StoredBytes
			if block.Encoding == EncodingRaw {
				file.raw += block.StoredBytes
			}
		}
	}
	return usage, rows.Err()
}

// choosePackRewrite picks the files whose rewrite frees the most per byte
// copied, while the copies fit in allowance. Once the generation would hold
// more files than limit, the smallest kept files are merged too.
func choosePackRewrite(usage map[string]*packUsage, allowance int64, limit int, packBytes int64, newBytes int64) rewritePlan {
	plan := rewritePlan{rewrite: make(map[string]bool)}
	type candidate struct {
		name    string
		benefit int64
		live    int64
	}
	var candidates []candidate
	for name, file := range usage {
		dead := max(0, file.size-file.live)
		benefit := dead + file.raw*rawRecompressionShare/100
		if benefit*100 >= file.size*rewriteBenefitShare && benefit > 0 {
			candidates = append(candidates, candidate{name: name, benefit: benefit, live: file.live})
		}
	}
	// Highest benefit per copied byte first; ties keep a stable order.
	sort.Slice(candidates, func(i, j int) bool {
		left := float64(candidates[i].benefit) / float64(max(candidates[i].live, 1))
		right := float64(candidates[j].benefit) / float64(max(candidates[j].live, 1))
		if left != right {
			return left > right
		}
		return candidates[i].name < candidates[j].name
	})
	for _, candidate := range candidates {
		if plan.copyBytes > math.MaxInt64-candidate.live || plan.copyBytes+candidate.live > allowance {
			continue
		}
		plan.rewrite[candidate.name] = true
		plan.copyBytes += candidate.live
	}
	kept := make([]string, 0, len(usage))
	for name := range usage {
		if !plan.rewrite[name] {
			kept = append(kept, name)
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		if usage[kept[i]].size != usage[kept[j]].size {
			return usage[kept[i]].size < usage[kept[j]].size
		}
		return kept[i] < kept[j]
	})
	files := func() int {
		written := plan.copyBytes + newBytes
		return len(kept) + int((written+packBytes-1)/max(packBytes, 1))
	}
	for len(kept) > 0 && files() > limit {
		smallest := usage[kept[0]]
		if plan.copyBytes+smallest.live > allowance {
			break
		}
		plan.rewrite[kept[0]] = true
		plan.copyBytes += smallest.live
		kept = kept[1:]
	}
	plan.full = len(kept) == 0
	return plan
}

// packRewriteAllowance is rewriteAllowance outside tests.
var packRewriteAllowance = rewriteAllowance

// rewriteAllowance is how many bytes a build may copy out of old pack files
// on top of the new data, keeping the store's free-space reserve.
func rewriteAllowance(store string, newBytes int64) (int64, error) {
	limits, err := storage.LoadLimits(store)
	if err != nil {
		return 0, err
	}
	available, err := storage.AvailableBytes(store)
	if err != nil {
		return 0, err
	}
	return max(0, available-limits.FreeSpaceReserveBytes-newBytes-rewriteSafetyMargin), nil
}

// GenerationWaste reports the bytes of the current generation's pack files
// and how many of them no live block uses.
func GenerationWaste(store string) (fileBytes int64, wastedBytes int64, err error) {
	generation, err := CurrentGeneration(store)
	if err != nil {
		return 0, 0, err
	}
	directory := filepath.Join(store, "packs", generation)
	index, err := openIndexV3(directory)
	if err != nil {
		return 0, 0, err
	}
	defer index.close()
	var live int64
	reader := bufio.NewReaderSize(io.NewSectionReader(index.blocks, 0, index.meta.BlockCount*blockV3RecordBytes), 1<<20)
	var record [blockV3RecordBytes]byte
	for position := int64(0); position < index.meta.BlockCount; position++ {
		if _, err := io.ReadFull(reader, record[:]); err != nil {
			return 0, 0, err
		}
		live += int64(binary.BigEndian.Uint64(record[12:20]))
	}
	for _, name := range index.meta.Packs {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil {
			return 0, 0, err
		}
		fileBytes += info.Size()
	}
	return fileBytes, max(0, fileBytes-live), nil
}
