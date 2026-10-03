package pack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/fold"
)

// PublishedLogicalBytes reads the immutable manifest archive associated with a
// published pack. Newly folded but not yet packed manifests must not inflate
// its apparent compression saving. This never opens a repairing resolver.
func PublishedLogicalBytes(ctx context.Context, store, generation string) (int64, error) {
	if !safeGeneration(generation) {
		return 0, errors.New("safe published generation is required")
	}
	directory := filepath.Join(store, "packs", generation)
	if _, err := readPublishedGeneration(directory); err != nil {
		return 0, err
	}
	catalog, err := ReadRecoveryCatalog(directory)
	if err != nil {
		return 0, err
	}
	wanted := make(map[string]RecoveryFile)
	for _, file := range catalog.Files {
		if strings.HasPrefix(file.Path, "manifests/") {
			wanted[file.Path] = file
		}
	}
	archive, closeArchive, err := openRecoveryArchive(directory)
	if err != nil {
		return 0, err
	}
	defer closeArchive()
	var total int64
	seenIDs := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		identity, ok := wanted[header.Name]
		if !ok {
			continue
		}
		if header.Size != identity.Bytes || header.Size < 0 || header.Size > 64<<20 {
			return 0, errors.New("invalid accounting manifest size")
		}
		data, err := io.ReadAll(io.LimitReader(archive, header.Size+1))
		if err != nil {
			return 0, err
		}
		digest := sha256.Sum256(data)
		if int64(len(data)) != identity.Bytes || hex.EncodeToString(digest[:]) != identity.SHA256 {
			return 0, errors.New("accounting manifest does not match publication proof")
		}
		manifest, err := fold.DecodeManifest(data)
		if err != nil {
			return 0, err
		}
		if seenIDs[manifest.Session.ID] || manifest.Source.Bytes < 0 || manifest.Source.Bytes > maxInt64()-total {
			return 0, errors.New("invalid accounting session total")
		}
		seenIDs[manifest.Session.ID] = true
		total += manifest.Source.Bytes
		delete(wanted, header.Name)
		if len(wanted) == 0 {
			break
		}
	}
	if len(wanted) != 0 {
		return 0, fmt.Errorf("published archive is missing %d accounting manifests", len(wanted))
	}
	return total, nil
}
