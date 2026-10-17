package tray

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
)

func TestSpaceAccountingSeparatesCompressionAndUnpublishedCopies(t *testing.T) {
	oldLimits := storage.DefaultLimits
	storage.DefaultLimits.FreeSpaceReserveBytes = 1 << 20
	t.Cleanup(func() { storage.DefaultLimits = oldLimits })
	store := t.TempDir()
	native := t.TempDir()
	objects := fold.NewObjectStore(store)
	write := func(id string, payload []byte) {
		t.Helper()
		ref, _, err := objects.Put(payload, true)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		original := filepath.Join(native, id+".jsonl")
		if err := os.WriteFile(original, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		manifest := fold.Manifest{Version: fold.ManifestVersion, Kind: fold.ManifestKind, Session: fold.ManifestSession{ID: id, RolloutPath: original}, Parts: []fold.Part{{Kind: fold.PartResidual, Object: ref}}}
		manifest.Source.Bytes = int64(len(payload))
		manifest.Source.SHA256 = hex.EncodeToString(digest[:])
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(store, "manifests"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, "manifests", id+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := bytes.Repeat([]byte("published-compressed-data"), 20000)
	write("first", first)
	if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := readSpaceAccounting(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	write("pending", bytes.Repeat([]byte("unpublished-extra-data"), 20000))
	after, err := readSpaceAccounting(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if before.CompressionLogical != int64(len(first)) || after.CompressionLogical != before.CompressionLogical || after.Compressed != before.Compressed {
		t.Fatalf("unpublished work changed compression: before=%+v after=%+v", before, after)
	}
	if after.Physical <= before.Physical || after.Pending <= before.Pending {
		t.Fatalf("extra copies not exposed: before=%+v after=%+v", before, after)
	}
}
