package pack

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestBuildAndResolverReadExactRandomRanges(t *testing.T) {
	root := t.TempDir()
	large := bytes.Repeat([]byte("large-object-block-"), 50000)
	refs := putObjects(t, root, []byte("shared-small-object"), large)
	writeManifest(t, root, "first", []fold.ObjectRef{refs[0], refs[1], refs[0]})
	writeManifest(t, root, "fork", []fold.ObjectRef{refs[0], refs[1]})

	result, err := Build(context.Background(), root, BuildOptions{BlockBytes: 256 << 10, PackBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if result.ObjectCount != 2 || result.BlockCount < 3 || result.PackCount < 1 {
		t.Fatalf("unexpected build result: %#v", result)
	}
	current, err := CurrentGeneration(root)
	if err != nil || current != result.Generation {
		t.Fatalf("current generation = %q, %v; want %q", current, err, result.Generation)
	}
	loose := fold.NewObjectStore(root)
	for _, ref := range refs {
		if err := os.Remove(loose.ObjectPath(ref.SHA256)); err != nil {
			t.Fatalf("remove loose object after pack build: %v", err)
		}
	}

	resolver, err := Open(root, OpenOptions{CacheBytes: 512 << 10})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	if resolver.Generation() != result.Generation {
		t.Fatalf("resolver generation = %q, want %q", resolver.Generation(), result.Generation)
	}

	for _, test := range []struct {
		name string
		ref  fold.ObjectRef
		data []byte
		off  int64
		size int
	}{
		{name: "small", ref: refs[0], data: []byte("shared-small-object"), off: 2, size: 8},
		{name: "large-first", ref: refs[1], data: large, off: 17, size: 333},
		{name: "large-block-boundary", ref: refs[1], data: large, off: (256 << 10) - 31, size: 1000},
		{name: "large-tail", ref: refs[1], data: large, off: int64(len(large) - 101), size: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			buffer := make([]byte, test.size)
			n, readErr := resolver.ReadAt(context.Background(), test.ref, buffer, test.off)
			end := int(test.off) + test.size
			if end > len(test.data) {
				end = len(test.data)
			}
			want := test.data[int(test.off):end]
			if !bytes.Equal(buffer[:n], want) {
				t.Fatalf("ReadAt bytes differ: got=%d want=%d", n, len(want))
			}
			if len(want) < test.size && !errors.Is(readErr, io.EOF) {
				t.Fatalf("ReadAt error = %v, want EOF", readErr)
			}
		})
	}
}

func TestDoctorAcceptsPristineBootstrapStoreButRejectsPartialContent(t *testing.T) {
	root := t.TempDir()
	report, err := Doctor(context.Background(), root)
	if err != nil || report.IssueCount != 0 || report.Generation != "" {
		t.Fatalf("pristine store doctor = %#v, %v", report, err)
	}
	bootstrap, err := IsBootstrapStore(root)
	if err != nil || !bootstrap {
		t.Fatalf("pristine bootstrap state = %t, %v", bootstrap, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "objects", "partial.zst"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	bootstrap, err = IsBootstrapStore(root)
	if err != nil || bootstrap {
		t.Fatalf("partial bootstrap state = %t, %v", bootstrap, err)
	}
	report, err = Doctor(context.Background(), root)
	if err != nil || report.IssueCount != 1 {
		t.Fatalf("partial store doctor = %#v, %v", report, err)
	}
}

func TestBuildWritesBoundedV3IndexAndResolverKeepsNoObjectMap(t *testing.T) {
	root := t.TempDir()
	values := make([][]byte, 0, 256)
	for index := 0; index < 256; index++ {
		values = append(values, []byte(fmt.Sprintf("bounded-v3-object-%06d", index)))
	}
	refs := putObjects(t, root, values...)
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{BlockBytes: 4 << 10})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "packs", result.Generation)
	if _, err := os.Stat(filepath.Join(directory, "index.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v3 build emitted legacy JSON index: %v", err)
	}
	metaData, err := os.ReadFile(filepath.Join(directory, indexV3MetaFilename))
	if err != nil {
		t.Fatal(err)
	}
	var meta indexV3Meta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatal(err)
	}
	objectsInfo, err := os.Stat(filepath.Join(directory, indexV3ObjectsFile))
	if err != nil {
		t.Fatal(err)
	}
	blocksInfo, err := os.Stat(filepath.Join(directory, indexV3BlocksFile))
	if err != nil {
		t.Fatal(err)
	}
	if meta.ObjectCount != int64(len(refs)) || objectsInfo.Size() != meta.ObjectCount*objectV3RecordBytes || blocksInfo.Size() != meta.BlockCount*blockV3RecordBytes {
		t.Fatalf("v3 fixed index sizes: meta=%#v objects=%d blocks=%d", meta, objectsInfo.Size(), blocksInfo.Size())
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if resolver.v3 == nil || len(resolver.objects) != 0 || resolver.ObjectCount() != int64(len(refs)) {
		t.Fatalf("v3 resolver retained legacy map: v3=%t map=%d count=%d", resolver.v3 != nil, len(resolver.objects), resolver.ObjectCount())
	}
}

func TestResolverRepairsUnsortedV3ObjectIndexFromRecoveryArchive(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("unsorted-a"), []byte("unsorted-b"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "packs", result.Generation, indexV3ObjectsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := append([]byte(nil), data[:objectV3RecordBytes]...)
	copy(data[:objectV3RecordBytes], data[objectV3RecordBytes:2*objectV3RecordBytes])
	copy(data[objectV3RecordBytes:2*objectV3RecordBytes], first)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("repair unsorted v3 index: %v", err)
	}
	defer resolver.Close()
	for index, ref := range refs {
		buffer := make([]byte, ref.RawBytes)
		if _, err := resolver.ReadAt(context.Background(), ref, buffer, 0); err != nil {
			t.Fatalf("read repaired object %d: %v", index, err)
		}
	}
	repaired, err := os.ReadFile(path)
	if err != nil || bytes.Equal(repaired, data) {
		t.Fatalf("runtime index was not restored: equal_corrupt=%t err=%v", bytes.Equal(repaired, data), err)
	}
}

func TestResolverRefusesIndexRepairWhenRecoveryArchiveIsCorrupt(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("protected-index-a"), []byte("protected-index-b"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "packs", result.Generation)
	if err := os.WriteFile(filepath.Join(directory, indexV3ObjectsFile), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, recoveryArchiveFilename), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, OpenOptions{}); err == nil {
		t.Fatal("resolver repaired an index without a valid recovery archive")
	}
}

func TestOpenRecoversCurrentIndexAndManifests(t *testing.T) {
	root := t.TempDir()
	value := bytes.Repeat([]byte("recoverable-session\n"), 200)
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	persistManagedManifestFixture(t, root, "session", value)
	directory := filepath.Join(root, "packs", result.Generation)
	for _, path := range []string{
		filepath.Join(root, "packs", "CURRENT"),
		filepath.Join(directory, indexV3MetaFilename),
		filepath.Join(directory, indexV3ObjectsFile),
		filepath.Join(directory, indexV3BlocksFile),
		filepath.Join(root, "manifests", "session.json"),
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("recover current pack: %v", err)
	}
	defer resolver.Close()
	if _, err := RepairCurrentManifests(root); err != nil {
		t.Fatalf("recover manifests: %v", err)
	}
	buffer := make([]byte, len(value))
	if _, err := resolver.ReadAt(context.Background(), refs[0], buffer, 0); err != nil || !bytes.Equal(buffer, value) {
		t.Fatalf("read recovered bytes: equal=%t err=%v", bytes.Equal(buffer, value), err)
	}
	manifest, err := fold.LoadManifest(root, "session")
	if err != nil || manifest.Source.Bytes != int64(len(value)) {
		t.Fatalf("load recovered manifest: %#v err=%v", manifest.Source, err)
	}
}

func TestRepairCurrentManifestsDoesNotRestoreUnmanagedManifest(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("unmanaged-manifest"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	manifestPath := fold.ManifestPath(root, "session")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	restored, err := RepairCurrentManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 0 {
		t.Fatalf("restored unmanaged manifests = %d, want 0", restored)
	}
	if _, err := os.Lstat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unmanaged manifest was resurrected: %v", err)
	}
}

func TestRepairCurrentManifestsDoesNotResurrectManifestFirstDeletion(t *testing.T) {
	root := t.TempDir()
	value := []byte("manifest-first-explicit-deletion")
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	state := persistManagedManifestFixture(t, root, "session", value)
	tombstone, err := vfs.PublishSessionDeletion(root, state, "/sessions/2026/07/25/session.jsonl")
	if err != nil {
		t.Fatalf("publish deletion tombstone: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(tombstone.RetiredManifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tombstone.ManifestPath, tombstone.RetiredManifestPath); err != nil {
		t.Fatalf("stage manifest-first deletion crash phase: %v", err)
	}

	restored, err := RepairCurrentManifests(root)
	if err != nil {
		t.Fatalf("repair around pending deletion: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored tombstoned manifests = %d, want 0", restored)
	}
	if _, err := os.Lstat(tombstone.ManifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tombstoned manifest was resurrected: %v", err)
	}
	if got, err := os.ReadFile(tombstone.RetiredManifestPath); err != nil || len(got) == 0 {
		t.Fatalf("retired manifest changed during repair: bytes=%d err=%v", len(got), err)
	}
	completed, err := vfs.AdvanceSessionDeletion(root, tombstone)
	if err != nil || !completed {
		t.Fatalf("advance deletion after repair completed=%t err=%v", completed, err)
	}
	if _, err := os.Lstat(tombstone.SessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted session state remains live: %v", err)
	}
}

func TestRepairCurrentManifestsRestoresLiveSessionButSkipsPendingDeletion(t *testing.T) {
	root := t.TempDir()
	deletedValue := []byte("deleted-session-manifest")
	liveValue := []byte("live-session-manifest")
	writeManifest(t, root, "deleted", putObjects(t, root, deletedValue))
	writeManifest(t, root, "live", putObjects(t, root, liveValue))
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	deletedState := persistManagedManifestFixture(t, root, "deleted", deletedValue)
	persistManagedManifestFixture(t, root, "live", liveValue)
	tombstone, err := vfs.PublishSessionDeletion(root, deletedState, "/sessions/2026/07/25/deleted.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tombstone.RetiredManifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tombstone.ManifestPath, tombstone.RetiredManifestPath); err != nil {
		t.Fatal(err)
	}
	liveManifestPath := fold.ManifestPath(root, "live")
	if err := os.Remove(liveManifestPath); err != nil {
		t.Fatal(err)
	}

	restored, err := RepairCurrentManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatalf("restored manifests = %d, want only the live session", restored)
	}
	if _, err := os.Lstat(tombstone.ManifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending deletion manifest was resurrected: %v", err)
	}
	liveManifest, err := fold.LoadManifestPath(liveManifestPath)
	if err != nil || liveManifest.Session.ID != "live" || liveManifest.Source.SHA256 != digestBytes(liveValue) {
		t.Fatalf("live manifest was not restored exactly: session=%q sha=%q err=%v", liveManifest.Session.ID, liveManifest.Source.SHA256, err)
	}
	if completed, err := vfs.AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("advance pending deletion completed=%t err=%v", completed, err)
	}
}

func TestRepairCurrentManifestsFailsClosedOnUnreadableDeletionTombstone(t *testing.T) {
	root := t.TempDir()
	value := []byte("unreadable-deletion-tombstone")
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	persistManagedManifestFixture(t, root, "session", value)
	manifestPath := fold.ManifestPath(root, "session")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	tombstonePath := vfs.SessionDeletionPath(root, "session")
	if err := os.MkdirAll(filepath.Dir(tombstonePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tombstonePath, []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(root, "packs", "CURRENT")
	if err := os.Remove(currentPath); err != nil {
		t.Fatal(err)
	}

	if restored, err := RepairCurrentManifests(root); err == nil {
		t.Fatalf("manifest repair accepted an unreadable tombstone: restored=%d", restored)
	}
	if _, err := os.Lstat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest was restored without valid deletion discovery: %v", err)
	}
	if _, err := os.Lstat(currentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pack CURRENT was recovered before deletion discovery succeeded: %v", err)
	}
}

func TestBuildRejectsRecoveryManifestWithWrongSourceDigest(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("manifest-source-proof"))
	writeManifest(t, root, "session", refs)
	manifest, err := fold.LoadManifest(root, "session")
	if err != nil {
		t.Fatal(err)
	}
	manifest.Source.SHA256 = strings.Repeat("0", sha256.Size*2)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fold.ManifestPath(root, "session"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err == nil {
		t.Fatal("pack build published a recovery archive whose manifest cannot reconstruct its declared source")
	}
	if _, err := os.Lstat(filepath.Join(root, "packs", publicationHeadFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed build published an authoritative head: %v", err)
	}
}

func TestRecoverCurrentDoesNotPromoteUnpublishedGeneration(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("published-generation"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop before publication")
	if _, err := Build(context.Background(), root, BuildOptions{BeforePublish: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("interrupted build error = %v, want %v", err, stop)
	}
	if err := os.Remove(filepath.Join(root, "packs", "CURRENT")); err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverCurrentGeneration(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != first.Generation {
		t.Fatalf("recovered generation = %s, want last authoritative %s", recovered, first.Generation)
	}
}

func TestOpenKeepsServingCurrentDuringPublicationHeadWindow(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("publication-window"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{})
	release := make(chan struct{})
	buildDone := make(chan error, 1)
	go func() {
		_, err := Build(context.Background(), root, BuildOptions{AfterPublicationHead: func() error {
			close(reached)
			<-release
			return nil
		}})
		buildDone <- err
	}()
	<-reached
	resolver, openErr := Open(root, OpenOptions{})
	if openErr == nil {
		if resolver.Generation() != first.Generation {
			openErr = fmt.Errorf("resolver generation = %s, want still-current %s", resolver.Generation(), first.Generation)
		}
		_ = resolver.Close()
	}
	close(release)
	buildErr := <-buildDone
	if openErr != nil {
		t.Fatalf("Open during publication window: %v", openErr)
	}
	if buildErr != nil {
		t.Fatalf("Build after publication window: %v", buildErr)
	}
}

func TestRecoverCurrentRollsForwardUniqueMarkerSuccessor(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("uncommitted-marker"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop before marker")
	if _, err := Build(context.Background(), root, BuildOptions{BeforePublish: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("interrupted build error = %v, want %v", err, stop)
	}
	entries, err := os.ReadDir(filepath.Join(root, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	var candidate string
	for _, entry := range entries {
		if entry.IsDir() && safeGeneration(entry.Name()) && entry.Name() != first.Generation {
			candidate = entry.Name()
			break
		}
	}
	if candidate == "" {
		t.Fatal("interrupted build did not leave a candidate generation")
	}
	if _, err := markGenerationPublished(filepath.Join(root, "packs", candidate), 2, first.Generation); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "packs", "CURRENT")); err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverCurrentGeneration(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != candidate {
		t.Fatalf("recovered generation = %s, want unique prepared successor %s", recovered, candidate)
	}
	head, _, err := readPublicationHead(filepath.Join(root, "packs"))
	if err != nil || head.Generation != candidate || head.Sequence != 2 {
		t.Fatalf("rolled-forward publication head=%#v err=%v", head, err)
	}
}

func TestBuildContinuesAfterRecoveringUniqueMarkerSuccessor(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("build-after-successor-recovery"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop before marker")
	if _, err := Build(context.Background(), root, BuildOptions{BeforePublish: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("interrupted build error = %v, want %v", err, stop)
	}
	entries, err := os.ReadDir(filepath.Join(root, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	var successor string
	for _, entry := range entries {
		if entry.IsDir() && safeGeneration(entry.Name()) && entry.Name() != first.Generation {
			successor = entry.Name()
			break
		}
	}
	if successor == "" {
		t.Fatal("interrupted build did not leave a candidate generation")
	}
	if _, err := markGenerationPublished(filepath.Join(root, "packs", successor), 2, first.Generation); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "packs", "CURRENT")); err != nil {
		t.Fatal(err)
	}

	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("build after successor recovery: %v", err)
	}
	head, marker, err := readPublicationHead(filepath.Join(root, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	if head.Generation != result.Generation || head.Sequence != 3 || marker.PreviousGeneration != successor {
		t.Fatalf("publication after recovered build: head=%#v marker=%#v successor=%s result=%s", head, marker, successor, result.Generation)
	}
}

func TestRecoverCurrentRefusesForkedMarkerSuccessors(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("forked-publication-marker"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop before marker")
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := Build(context.Background(), root, BuildOptions{BeforePublish: func() error { return stop }}); !errors.Is(err, stop) {
			t.Fatalf("interrupted build %d error = %v", attempt, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, entry := range entries {
		if !entry.IsDir() || !safeGeneration(entry.Name()) || entry.Name() == first.Generation {
			continue
		}
		if _, err := markGenerationPublished(filepath.Join(root, "packs", entry.Name()), 2, first.Generation); err != nil {
			t.Fatal(err)
		}
		marked++
	}
	if marked != 2 {
		t.Fatalf("marked successor count = %d, want 2", marked)
	}
	if err := os.Remove(filepath.Join(root, "packs", "CURRENT")); err != nil {
		t.Fatal(err)
	}
	if recovered, err := RecoverCurrentGeneration(context.Background(), root); err == nil {
		t.Fatalf("forked publication successors recovered as %s", recovered)
	}
	if _, err := os.Lstat(filepath.Join(root, "packs", "CURRENT")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forked recovery republished CURRENT: %v", err)
	}
}

func TestRecoverCurrentRefusesCorruptLatestPublishedGeneration(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("no-published-downgrade"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	latest, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "packs", "CURRENT")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packs", latest.Generation, recoveryArchiveFilename), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if recovered, err := RecoverCurrentGeneration(context.Background(), root); err == nil {
		t.Fatalf("corrupt latest published generation recovered as %s", recovered)
	}
	if _, err := os.Lstat(filepath.Join(root, "packs", "CURRENT")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed recovery republished CURRENT: %v", err)
	}
}

func TestRecoverCurrentRefusesMissingLatestPublishedDirectory(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("missing-latest-generation"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "packs", "CURRENT")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "packs", latest.Generation)); err != nil {
		t.Fatal(err)
	}
	if recovered, err := RecoverCurrentGeneration(context.Background(), root); err == nil {
		t.Fatalf("missing latest directory downgraded to %s (previous %s)", recovered, first.Generation)
	}
	if _, err := os.Lstat(filepath.Join(root, "packs", "CURRENT")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed recovery republished CURRENT: %v", err)
	}
}

func TestBuildMigratesLegacyCurrentWithoutPublicationHead(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("legacy-publication-migration"))
	writeManifest(t, root, "session", refs)
	legacy, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(root, "packs", legacy.Generation)
	for _, path := range []string{
		filepath.Join(root, "packs", publicationHeadFilename),
		filepath.Join(legacyDir, publishedMarkerFilename),
		filepath.Join(legacyDir, recoveryArchiveFilename),
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	next, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("build after legacy publication migration: %v", err)
	}
	head, marker, err := readPublicationHead(filepath.Join(root, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	if head.Generation != next.Generation || head.Sequence != 2 || marker.PreviousGeneration != legacy.Generation {
		t.Fatalf("migrated publication chain: head=%#v marker=%#v legacy=%s next=%s", head, marker, legacy.Generation, next.Generation)
	}
}

func TestBuildMigratesLegacyCurrentAfterLaterFold(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("legacy-packed-source"))
	writeManifest(t, root, "packed-session", refs)
	legacy, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(root, "packs", legacy.Generation)
	for _, path := range []string{
		filepath.Join(root, "packs", publicationHeadFilename),
		filepath.Join(legacyDir, publishedMarkerFilename),
		filepath.Join(legacyDir, recoveryArchiveFilename),
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	later := putObjects(t, root, []byte("loose-object-after-legacy-pack"))
	writeManifest(t, root, "packed-session", later)
	next, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("build after folding a live manifest the legacy pack does not contain: %v", err)
	}
	if next.Generation == legacy.Generation {
		t.Fatal("later fold reused the legacy pack generation")
	}
}

func TestBuildResolvesLegacyPublicationChainBeforePublishingHead(t *testing.T) {
	t.Run("unique complete chain", func(t *testing.T) {
		root := t.TempDir()
		refs := putObjects(t, root, []byte("complete-publication-chain"))
		writeManifest(t, root, "session", refs)
		first, err := Build(context.Background(), root, BuildOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
			t.Fatal(err)
		}
		third, err := Build(context.Background(), root, BuildOptions{})
		if err != nil {
			t.Fatal(err)
		}
		packsDir := filepath.Join(root, "packs")
		if err := os.Remove(filepath.Join(packsDir, publicationHeadFilename)); err != nil {
			t.Fatal(err)
		}
		if err := publishCurrent(packsDir, first.Generation); err != nil {
			t.Fatal(err)
		}

		result, err := Build(context.Background(), root, BuildOptions{})
		if err != nil {
			t.Fatalf("build after reconstructing complete publication chain: %v", err)
		}
		head, marker, err := readPublicationHead(packsDir)
		if err != nil {
			t.Fatal(err)
		}
		if head.Generation != result.Generation || head.Sequence != 4 || marker.PreviousGeneration != third.Generation {
			t.Fatalf("reconstructed publication chain: head=%#v marker=%#v third=%s result=%s", head, marker, third.Generation, result.Generation)
		}
	})

	for _, test := range []struct {
		name           string
		candidateCount int
		sequence       uint64
	}{
		{name: "fork", candidateCount: 2, sequence: 2},
		{name: "sequence gap", candidateCount: 1, sequence: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			refs := putObjects(t, root, []byte("invalid-publication-chain"))
			writeManifest(t, root, "session", refs)
			first, err := Build(context.Background(), root, BuildOptions{})
			if err != nil {
				t.Fatal(err)
			}
			stop := errors.New("stop before marker")
			for attempt := 0; attempt < test.candidateCount; attempt++ {
				if _, err := Build(context.Background(), root, BuildOptions{BeforePublish: func() error { return stop }}); !errors.Is(err, stop) {
					t.Fatalf("interrupted build %d error = %v, want %v", attempt, err, stop)
				}
			}
			packsDir := filepath.Join(root, "packs")
			entries, err := os.ReadDir(packsDir)
			if err != nil {
				t.Fatal(err)
			}
			marked := 0
			for _, entry := range entries {
				if !entry.IsDir() || !safeGeneration(entry.Name()) || entry.Name() == first.Generation {
					continue
				}
				if _, err := markGenerationPublished(filepath.Join(packsDir, entry.Name()), test.sequence, first.Generation); err != nil {
					t.Fatal(err)
				}
				marked++
			}
			if marked != test.candidateCount {
				t.Fatalf("marked candidate count = %d, want %d", marked, test.candidateCount)
			}
			if err := os.Remove(filepath.Join(packsDir, publicationHeadFilename)); err != nil {
				t.Fatal(err)
			}

			if _, err := Build(context.Background(), root, BuildOptions{}); err == nil {
				t.Fatal("build accepted an invalid publication chain")
			}
			if _, err := os.Lstat(filepath.Join(packsDir, publicationHeadFilename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid publication chain persisted PUBLISHED: %v", err)
			}
		})
	}
}

func TestRepairCurrentManifestsDoesNotResurrectRemovedSession(t *testing.T) {
	root := t.TempDir()
	value := []byte("removed-managed-session")
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	persistManagedManifestFixture(t, root, "session", value)
	manifestPath := fold.ManifestPath(root, "session")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "fs", "sessions", "session")); err != nil {
		t.Fatal(err)
	}
	restored, err := RepairCurrentManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 0 {
		t.Fatalf("restored removed session manifests = %d, want 0", restored)
	}
	if _, err := os.Lstat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed session manifest was resurrected: %v", err)
	}
}

func TestRepairCurrentManifestsRequiresExactRepublishedManifestVersion(t *testing.T) {
	root := t.TempDir()
	value := []byte("managed-manifest-metadata-version")
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	persistManagedManifestFixture(t, root, "session", value)

	manifestPath := fold.ManifestPath(root, "session")
	manifest, err := fold.LoadManifestPath(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Session.Title = "metadata version B"
	versionB, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	versionB = append(versionB, '\n')
	if err := os.WriteFile(manifestPath, versionB, 0o600); err != nil {
		t.Fatal(err)
	}
	if restored, err := RepairCurrentManifests(root); err == nil {
		t.Fatalf("stale state accepted metadata version B: restored=%d", restored)
	}
	if current, err := os.ReadFile(manifestPath); err != nil || !bytes.Equal(current, versionB) {
		t.Fatalf("failed repair changed metadata version B: equal=%t err=%v", bytes.Equal(current, versionB), err)
	}

	statePath := filepath.Join(root, "fs", "sessions", "session", "state.json")
	if _, err := vfs.RepublishSessionState(statePath); err != nil {
		t.Fatalf("republish metadata version B: %v", err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatalf("build republished metadata version B: %v", err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	restored, err := RepairCurrentManifests(root)
	if err != nil {
		t.Fatalf("restore republished metadata version B: %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored manifests = %d, want 1", restored)
	}
	if current, err := os.ReadFile(manifestPath); err != nil || !bytes.Equal(current, versionB) {
		t.Fatalf("restored manifest is not metadata version B: equal=%t err=%v", bytes.Equal(current, versionB), err)
	}
}

func TestRepairCurrentManifestsCannotFollowIntermediateSymlinkOutsideStore(t *testing.T) {
	root := t.TempDir()
	value := []byte("managed-manifest-symlink-boundary")
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	persistManagedManifestFixture(t, root, "session", value)
	manifestPath := fold.ManifestPath(root, "session")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	manifestRoot := filepath.Dir(manifestPath)
	if err := os.Remove(manifestRoot); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, manifestRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if restored, err := RepairCurrentManifests(root); err == nil {
		t.Fatalf("recovery followed an intermediate symlink outside the store: restored=%d", restored)
	}
	if _, err := os.Lstat(filepath.Join(outside, "session.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery wrote outside the store: %v", err)
	}
}

func TestRepairGenerationIndexCannotFollowGenerationSymlinkOutsideStore(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("generation-root-symlink"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	generationDir := filepath.Join(root, "packs", result.Generation)
	outside := t.TempDir()
	outsideGeneration := filepath.Join(outside, result.Generation)
	if err := os.Rename(generationDir, outsideGeneration); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideGeneration, generationDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	indexPath := filepath.Join(outsideGeneration, indexV3ObjectsFile)
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := repairGenerationIndex(generationDir); err == nil {
		t.Fatal("generation symlink outside store allowed index recovery")
	}
	if _, err := os.Lstat(indexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generation symlink recovery wrote outside store: %v", err)
	}
}

func TestOpenRejectsGenerationSymlinkOutsideStore(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("generation-read-root-symlink"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	generationDir := filepath.Join(root, "packs", result.Generation)
	outside := t.TempDir()
	outsideGeneration := filepath.Join(outside, result.Generation)
	if err := os.Rename(generationDir, outsideGeneration); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideGeneration, generationDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	resolver, err := Open(root, OpenOptions{})
	if resolver != nil {
		_ = resolver.Close()
	}
	if err == nil {
		t.Fatal("resolver followed a generation symlink outside the store")
	}
	if info, statErr := os.Lstat(generationDir); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("generation symlink was replaced: mode=%v err=%v", infoMode(info), statErr)
	}
	if _, statErr := os.Stat(filepath.Join(outsideGeneration, indexV3MetaFilename)); statErr != nil {
		t.Fatalf("outside generation changed: %v", statErr)
	}
}

func TestOpenRejectsExternalV3IndexSymlinks(t *testing.T) {
	for _, name := range []string{indexV3MetaFilename, indexV3ObjectsFile, indexV3BlocksFile} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			refs := putObjects(t, root, []byte("external-index-symlink-"+name))
			writeManifest(t, root, "session", refs)
			result, err := Build(context.Background(), root, BuildOptions{})
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "packs", result.Generation, name)
			want, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(outside, want, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			resolver, err := Open(root, OpenOptions{})
			if resolver != nil {
				_ = resolver.Close()
			}
			if err == nil {
				t.Fatalf("resolver followed external %s symlink", name)
			}
			if info, statErr := os.Lstat(target); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("index symlink was replaced: mode=%v err=%v", infoMode(info), statErr)
			}
			if got, readErr := os.ReadFile(outside); readErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("outside index changed: equal=%t err=%v", bytes.Equal(got, want), readErr)
			}
		})
	}
}

func TestOpenRejectsExternalPackFileSymlink(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("external-pack-file-symlink"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "packs", result.Generation, "pack-000001.pack")
	want, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), filepath.Base(target))
	if err := os.WriteFile(outside, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := VerifyRecovery(context.Background(), root, result.Generation); err == nil {
		t.Fatal("recovery verification followed an external pack file symlink")
	}
	resolver, err := Open(root, OpenOptions{})
	if resolver != nil {
		_ = resolver.Close()
	}
	if err == nil {
		t.Fatal("resolver followed an external pack file symlink")
	}
	if info, statErr := os.Lstat(target); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pack symlink was replaced: mode=%v err=%v", infoMode(info), statErr)
	}
	if got, readErr := os.ReadFile(outside); readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("outside pack changed: equal=%t err=%v", bytes.Equal(got, want), readErr)
	}
}

func TestVerifyRecoveryRejectsExternalArchiveSymlink(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("external-recovery-archive-symlink"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "packs", result.Generation, recoveryArchiveFilename)
	want, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), recoveryArchiveFilename)
	if err := os.WriteFile(outside, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := VerifyRecovery(context.Background(), root, result.Generation); err == nil {
		t.Fatal("recovery verification followed an external recovery archive symlink")
	}
	if info, statErr := os.Lstat(target); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("recovery archive symlink was replaced: mode=%v err=%v", infoMode(info), statErr)
	}
	if got, readErr := os.ReadFile(outside); readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("outside recovery archive changed: equal=%t err=%v", bytes.Equal(got, want), readErr)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode()
}

func TestRecoveryManifestPublishDoesNotClobberConcurrentTarget(t *testing.T) {
	root := t.TempDir()
	data := []byte("manifest target appeared after validation")
	identity := RecoveryFile{Path: "manifests/session.json", Bytes: int64(len(data)), SHA256: digestBytes(data)}
	target, err := newRecoveryRestoreTarget(root, identity.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	err = restoreRecoveryEntryValidated(bytes.NewReader(data), target, identity, func(*os.File) error {
		return os.WriteFile(target.absolutePath(), []byte("concurrent target"), 0o600)
	})
	if err == nil {
		t.Fatal("manifest recovery clobbered a target that appeared after validation")
	}
	if got, readErr := os.ReadFile(target.absolutePath()); readErr != nil || string(got) != "concurrent target" {
		t.Fatalf("concurrent target changed: %q err=%v", got, readErr)
	}
}

func TestPackOnlyUnfoldAndFoldDoctor(t *testing.T) {
	root := t.TempDir()
	first := []byte("first packed object\n")
	second := bytes.Repeat([]byte("second packed object\n"), 1000)
	refs := putObjects(t, root, first, second)
	writeManifest(t, root, "session", []fold.ObjectRef{refs[0], refs[1], refs[0]})
	if _, err := Build(context.Background(), root, BuildOptions{BlockBytes: 4 << 10}); err != nil {
		t.Fatal(err)
	}
	store := fold.NewObjectStore(root)
	for _, ref := range refs {
		if err := os.Remove(store.ObjectPath(ref.SHA256)); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	target := filepath.Join(t.TempDir(), "restored.jsonl")
	result, err := fold.UnfoldWithOptions(context.Background(), root, "session", fold.UnfoldOptions{TargetPath: target, Reader: resolver})
	if err != nil || !result.Verified {
		t.Fatalf("pack-only unfold: result=%#v err=%v", result, err)
	}
	want := append(append(append([]byte(nil), first...), second...), first...)
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("restored bytes differ: err=%v", err)
	}
	doctor, err := fold.DoctorWithOptions(context.Background(), root, fold.DoctorOptions{Reader: resolver})
	if err != nil || doctor.IssueCount != 0 || doctor.VerifiedManifestCount != 1 {
		t.Fatalf("pack-only fold doctor: result=%#v err=%v", doctor, err)
	}
}

func TestFoldReusesPackedObjectWithoutRecreatingLooseCopy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "first.jsonl")
	data := bytes.Repeat([]byte("{\"packed_reuse\":\"same-large-value\"}\n"), 200)
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fold.Fold(context.Background(), fold.Session{ID: "first", RolloutPath: source, Archived: true}, fold.FoldOptions{StoreDir: root, Apply: true, FieldThreshold: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(root, "second.jsonl")
	if err := os.WriteFile(second, data, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := fold.Fold(context.Background(), fold.Session{ID: "second", RolloutPath: second, Archived: true}, fold.FoldOptions{StoreDir: root, Apply: true, FieldThreshold: 8, ExistingReader: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if result.UniqueObjects != 0 || result.ReusedObjects == 0 || result.NewStoredBytes != 0 {
		t.Fatalf("packed fold reuse = %#v", result)
	}
	looseFiles := 0
	_ = filepath.WalkDir(filepath.Join(root, "objects"), func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && filepath.Ext(entry.Name()) == ".zst" {
			looseFiles++
		}
		return err
	})
	if looseFiles != 0 {
		t.Fatalf("fold recreated %d loose object(s)", looseFiles)
	}
}

func TestRetireLooseRequiresPackOnlyProofAndReportsActualReclamation(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("retire-loose-object"), 1000))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	dry, err := RetireLoose(context.Background(), root, RetireLooseOptions{})
	if err != nil || !dry.DryRun || dry.CandidateCount != 1 || dry.RetiredCount != 0 {
		t.Fatalf("retire loose dry run: result=%#v err=%v", dry, err)
	}
	if _, err := os.Stat(fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)); err != nil {
		t.Fatalf("dry-run removed loose object: %v", err)
	}
	applied, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true})
	if err != nil || applied.RetiredCount != 1 || applied.RetiredBytes == 0 || applied.AuditPath == "" {
		t.Fatalf("retire loose apply: result=%#v err=%v", applied, err)
	}
	if _, err := os.Stat(fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loose object remains after retire: %v", err)
	}
	if _, err := os.Stat(applied.AuditPath); err != nil {
		t.Fatalf("retirement audit missing: %v", err)
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	target := filepath.Join(t.TempDir(), "restored.jsonl")
	if _, err := fold.UnfoldWithOptions(context.Background(), root, "session", fold.UnfoldOptions{TargetPath: target, Reader: resolver}); err != nil {
		t.Fatalf("unfold after loose retirement: %v", err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatalf("pack-only rebuild after loose retirement: %v", err)
	}
	if _, err := os.Stat(fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pack-only rebuild recreated loose object: %v", err)
	}
}

func TestRetireLooseRefusesCorruptPackBeforeDeletion(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("corrupt-before-retire"), 1000))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	index := loadCurrentIndex(t, root)
	block := index.Objects[0].Blocks[0]
	path := filepath.Join(root, "packs", index.Generation, block.Pack)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, block.PackOffset); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err == nil {
		t.Fatal("corrupt pack was accepted for loose retirement")
	}
	if _, err := os.Stat(fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)); err != nil {
		t.Fatalf("corrupt pack retirement removed loose recovery copy: %v", err)
	}
}

func TestRetireLooseRefusesDigestNamedFileWithDifferentDecodedBytes(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("packed authoritative bytes"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	store := fold.NewObjectStore(root)
	other, _, err := store.Put([]byte("different loose bytes"), true)
	if err != nil {
		t.Fatal(err)
	}
	wrongCompressed, err := os.ReadFile(store.ObjectPath(other.SHA256))
	if err != nil {
		t.Fatal(err)
	}
	candidate := store.ObjectPath(refs[0].SHA256)
	if err := os.WriteFile(candidate, wrongCompressed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.ObjectPath(other.SHA256)); err != nil {
		t.Fatal(err)
	}

	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err == nil {
		t.Fatal("digest-shaped loose filename authorized deletion of unrelated decoded bytes")
	}
	if _, err := os.Lstat(candidate); err != nil {
		t.Fatalf("unproved loose candidate was removed: %v", err)
	}
}

func TestRetireLooseRefusesSameSizeInPlaceRewriteAfterProof(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("retire loose stable bytes"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	candidate := fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)
	compressed, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(candidate)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RetireLoose(context.Background(), root, RetireLooseOptions{
		Apply: true,
		BeforeRemove: func(path string) error {
			if path != candidate {
				return nil
			}
			changed := append([]byte(nil), compressed...)
			changed[len(changed)/2] ^= 0x55
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				return err
			}
			return os.Chtimes(path, info.ModTime(), info.ModTime())
		},
	})
	if err == nil {
		t.Fatal("same-size in-place loose rewrite passed retirement final proof")
	}
	if current, statErr := os.Stat(candidate); statErr != nil || current.Size() != int64(len(compressed)) {
		t.Fatalf("changed loose candidate was not preserved: info=%v err=%v", current, statErr)
	}
}

func TestRetireLoosePreservesDigestShapedSymlink(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("packed symlink object"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	candidate := fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)
	outside := filepath.Join(t.TempDir(), "outside.zst")
	data, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, candidate); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err == nil {
		t.Fatal("digest-shaped loose symlink was not reported as unproved")
	}
	if info, err := os.Lstat(candidate); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("digest-shaped loose symlink was removed: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(outside); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("outside loose target changed: equal=%t err=%v", bytes.Equal(got, data), err)
	}
}

func TestRetireLoosePreservesValidObjectOutsideCanonicalShard(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("packed object copied into a foreign directory"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	store := fold.NewObjectStore(root)
	foreign := filepath.Join(root, "objects", "foreign", refs[0].SHA256+".zst")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(store.ObjectPath(refs[0].SHA256), foreign); err != nil {
		t.Fatal(err)
	}

	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err == nil {
		t.Fatal("noncanonical packed loose object was accepted for retirement")
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("foreign loose object was removed: %v", err)
	}
}

func TestRetireLooseRechecksCurrentBeforeDeletion(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("current-race-before-retire"), 1000))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	loose := fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)
	changed := false
	_, err = RetireLoose(context.Background(), root, RetireLooseOptions{
		Apply: true,
		BeforeRemove: func(string) error {
			if changed {
				return nil
			}
			changed = true
			return os.WriteFile(filepath.Join(root, "packs", "CURRENT"), []byte(first.Generation+"\n"), 0o600)
		},
	})
	if err == nil {
		t.Fatal("pack CURRENT race allowed loose retirement")
	}
	if _, statErr := os.Stat(loose); statErr != nil {
		t.Fatalf("loose object removed after CURRENT changed: %v", statErr)
	}
}

func TestRetireLooseRechecksPackedCandidateBeforeDeletion(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("packed candidate rewrite"), 1000))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	object, packed, err := resolver.lookupObject(refs[0].SHA256)
	if closeErr := resolver.Close(); err == nil {
		err = closeErr
	}
	if err != nil || !packed || len(object.Blocks) == 0 {
		t.Fatalf("resolve packed candidate: packed=%t object=%#v err=%v", packed, object, err)
	}
	block := object.Blocks[0]
	generation, err := CurrentGeneration(root)
	if err != nil {
		t.Fatal(err)
	}
	packPath := filepath.Join(root, "packs", generation, block.Pack)
	loose := fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)
	changed := false
	_, err = RetireLoose(context.Background(), root, RetireLooseOptions{
		Apply: true,
		BeforeRemove: func(string) error {
			if changed {
				return nil
			}
			changed = true
			file, err := os.OpenFile(packPath, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer file.Close()
			value := []byte{0}
			if _, err := file.ReadAt(value, block.PackOffset); err != nil {
				return err
			}
			value[0] ^= 0xff
			_, err = file.WriteAt(value, block.PackOffset)
			return err
		},
	})
	if err == nil {
		t.Fatal("packed object rewrite after proof allowed loose retirement")
	}
	if _, statErr := os.Stat(loose); statErr != nil {
		t.Fatalf("loose object removed after packed candidate changed: %v", statErr)
	}
}

func TestResolverReusesDecoderAcrossDistinctBlocks(t *testing.T) {
	root := t.TempDir()
	value := bytes.Repeat([]byte("decoder-pool-block-data"), 20000)
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{BlockBytes: 64 << 10, PackBytes: 1 << 20}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })

	created := 0
	resolver.decoderFactory = func() (*zstd.Decoder, error) {
		created++
		return newPackDecoder()
	}
	for _, offset := range []int64{0, 70 << 10} {
		if _, err := resolver.ReadAt(context.Background(), refs[0], make([]byte, 1), offset); err != nil {
			t.Fatalf("ReadAt(%d): %v", offset, err)
		}
	}
	if created != 1 {
		t.Fatalf("decoder creations = %d, want 1", created)
	}
}

func TestBlockCacheDoesNotRetainOversizedCallerBacking(t *testing.T) {
	cache := newBlockCache(64)
	value := make([]byte, 16, 4096)
	copy(value, []byte("sixteen-byte-val"))
	cache.put("object:0", value)

	cached, ok := cache.get("object:0")
	if !ok {
		t.Fatal("cache rejected a value within its byte budget")
	}
	if len(cached) != len(value) || cap(cached) != len(cached) {
		t.Fatalf("cached slice len=%d cap=%d, want exact owned storage", len(cached), cap(cached))
	}
	value[0] = 'X'
	if cached[0] == value[0] {
		t.Fatal("cache retained the caller's oversized backing array")
	}
	if cache.used != int64(len(cached)) {
		t.Fatalf("cache used=%d, want %d", cache.used, len(cached))
	}
}

func TestBuildSelectsRawAndZstdBlocksAndResolverReadsBoth(t *testing.T) {
	root := t.TempDir()
	raw := make([]byte, 8<<10)
	if _, err := cryptorand.Read(raw); err != nil {
		t.Fatal(err)
	}
	compressible := bytes.Repeat([]byte("highly-compressible-value"), 400)
	refs := putObjects(t, root, raw, compressible)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	index := loadCurrentIndex(t, root)
	encodings := make(map[string]string, len(index.Objects))
	for _, object := range index.Objects {
		if len(object.Blocks) != 1 {
			t.Fatalf("object %s blocks = %d, want 1", object.SHA256, len(object.Blocks))
		}
		encodings[object.SHA256] = object.Blocks[0].Encoding
	}
	if encodings[refs[0].SHA256] != EncodingRaw || encodings[refs[1].SHA256] != EncodingZstd {
		t.Fatalf("encodings = %#v", encodings)
	}

	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer resolver.Close()
	for index, want := range [][]byte{raw, compressible} {
		got := make([]byte, len(want))
		if n, err := resolver.ReadAt(context.Background(), refs[index], got, 0); n != len(want) || err != nil {
			t.Fatalf("ReadAt(%d) = %d, %v", index, n, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("ReadAt(%d) bytes differ", index)
		}
	}
}

func TestResolverOpensLegacyV1ZstdIndex(t *testing.T) {
	root := t.TempDir()
	value := bytes.Repeat([]byte("legacy-zstd-data"), 2000)
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	index := loadCurrentIndex(t, root)
	index.Version = legacyIndexVersion
	index.Kind = legacyIndexKind
	for objectIndex := range index.Objects {
		for blockIndex := range index.Objects[objectIndex].Blocks {
			block := &index.Objects[objectIndex].Blocks[blockIndex]
			if block.Encoding != EncodingZstd {
				t.Fatalf("legacy fixture block encoding = %q, want zstd", block.Encoding)
			}
			block.Encoding = ""
		}
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packs", result.Generation, "index.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{indexV3MetaFilename, indexV3ObjectsFile, indexV3BlocksFile} {
		if err := os.Remove(filepath.Join(root, "packs", result.Generation, name)); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open legacy v1: %v", err)
	}
	defer resolver.Close()
	got := make([]byte, len(value))
	if n, err := resolver.ReadAt(context.Background(), refs[0], got, 0); n != len(value) || err != nil || !bytes.Equal(got, value) {
		t.Fatalf("legacy ReadAt = %d, %v, equal=%t", n, err, bytes.Equal(got, value))
	}
}

func TestResolverSupportsOSCacheBypassOption(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("cache-bypass-object"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{BypassOSCache: true})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	buffer := make([]byte, refs[0].RawBytes)
	if _, err := resolver.ReadAt(context.Background(), refs[0], buffer, 0); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "cache-bypass-object" {
		t.Fatalf("unexpected bytes: %q", buffer)
	}
}

func TestResolverHoldsGenerationLeaseUntilClose(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("leased-generation"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaseDirectory := filepath.Join(root, "packs", result.Generation, "leases")
	active, err := storage.DirectoryHasActiveLease(leaseDirectory, false)
	if err != nil || !active {
		t.Fatalf("resolver generation lease: active=%t err=%v", active, err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}
	active, err = storage.DirectoryHasActiveLease(leaseDirectory, true)
	if err != nil || active {
		t.Fatalf("closed resolver generation lease: active=%t err=%v", active, err)
	}
}

func TestResolversShareGenerationResourcesUntilLastClose(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("shared-generation-resources"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root, OpenOptions{})
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if first.cache != second.cache || first.packs["pack-000001.pack"] != second.packs["pack-000001.pack"] {
		_ = first.Close()
		_ = second.Close()
		t.Fatal("resolvers for one generation did not share cache and pack descriptors")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	leaseDirectory := filepath.Join(root, "packs", result.Generation, "leases")
	active, err := storage.DirectoryHasActiveLease(leaseDirectory, false)
	if err != nil || !active {
		t.Fatalf("shared lease after first close: active=%t err=%v", active, err)
	}
	buffer := make([]byte, refs[0].RawBytes)
	if _, err := second.ReadAt(context.Background(), refs[0], buffer, 0); err != nil {
		t.Fatalf("second resolver after first close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	active, err = storage.DirectoryHasActiveLease(leaseDirectory, true)
	if err != nil || active {
		t.Fatalf("shared lease after final close: active=%t err=%v", active, err)
	}
}

func TestDoctorDoesNotPopulateActiveResolverCache(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("doctor-cache-isolation"), 1000))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if resolver.cache.used != 0 {
		t.Fatalf("new resolver cache bytes = %d", resolver.cache.used)
	}
	report, err := Doctor(context.Background(), root)
	if err != nil || report.IssueCount != 0 || report.ManifestCount != 1 || report.VerifiedManifestCount != 1 {
		t.Fatalf("Doctor: report=%#v err=%v", report, err)
	}
	if resolver.cache.used != 0 {
		t.Fatalf("doctor populated active resolver cache with %d bytes", resolver.cache.used)
	}
}

func TestDoctorDetectsManifestOrderingCorruptionWithValidObjects(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("first-object"), []byte("second-object"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifest(root, "session")
	if err != nil {
		t.Fatal(err)
	}
	manifest.Parts[0], manifest.Parts[1] = manifest.Parts[1], manifest.Parts[0]
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fold.ManifestPath(root, "session"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Doctor(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if report.IssueCount == 0 || report.VerifiedManifestCount != 0 {
		t.Fatalf("manifest ordering corruption passed doctor: %#v", report)
	}
}

func TestOpenDoesNotRecreateGenerationMissingFromCurrent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "packs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packs", "CURRENT"), []byte("gen-missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, OpenOptions{}); err == nil {
		t.Fatal("resolver unexpectedly opened a missing generation")
	}
	if _, err := os.Lstat(filepath.Join(root, "packs", "gen-missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing pack generation was recreated: %v", err)
	}
}

func TestValidatePackedBlockBoundsUsesCachedPackSizes(t *testing.T) {
	objectDigest := strings.Repeat("a", 64)
	blockDigest := strings.Repeat("b", 64)
	index := Index{
		Objects: []Object{{
			SHA256: objectDigest,
			Blocks: []Block{
				{Pack: "pack-000001.bin", PackOffset: 0, StoredBytes: 10, SHA256: blockDigest},
				{Pack: "pack-000001.bin", PackOffset: 10, StoredBytes: 20, SHA256: blockDigest},
			},
		}},
	}
	if err := validatePackedBlockBounds(index, map[string]int64{"pack-000001.bin": 30}); err != nil {
		t.Fatalf("valid packed bounds: %v", err)
	}
	if err := validatePackedBlockBounds(index, map[string]int64{"pack-000001.bin": 29}); err == nil {
		t.Fatal("truncated pack bounds were accepted")
	}
	if err := validatePackedBlockBounds(index, map[string]int64{}); err == nil {
		t.Fatal("missing pack bounds were accepted")
	}
}

func TestBuildInterruptionKeepsPreviousGenerationCurrent(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("first-generation"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("first Build returned error: %v", err)
	}

	stop := errors.New("stop before publish")
	if _, err := Build(context.Background(), root, BuildOptions{BeforePublish: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("interrupted Build error = %v, want %v", err, stop)
	}
	current, err := os.ReadFile(filepath.Join(root, "packs", "CURRENT"))
	if err != nil {
		t.Fatalf("read CURRENT: %v", err)
	}
	if string(bytes.TrimSpace(current)) != first.Generation {
		t.Fatalf("CURRENT = %q, want %q", bytes.TrimSpace(current), first.Generation)
	}

	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open previous generation: %v", err)
	}
	defer resolver.Close()
	buffer := make([]byte, refs[0].RawBytes)
	if _, err := resolver.ReadAt(context.Background(), refs[0], buffer, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read previous generation: %v", err)
	}
}

func TestBuildCancellationBeforePublicationRecoversWithoutMismatch(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("canceled-publication"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = Build(ctx, root, BuildOptions{BeforePublish: func() error {
		cancel()
		return ctx.Err()
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Build error = %v, want context.Canceled", err)
	}
	recovered, err := RecoverCurrentGeneration(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != first.Generation {
		t.Fatalf("recovered generation = %q, want %q", recovered, first.Generation)
	}
	if report, doctorErr := Doctor(context.Background(), root); doctorErr != nil || report.IssueCount != 0 || report.VerifiedManifestCount != 1 {
		t.Fatalf("doctor after canceled build recovery: report=%#v err=%v", report, doctorErr)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatalf("build after recovery: %v", err)
	}
	if report, doctorErr := Doctor(context.Background(), root); doctorErr != nil || report.IssueCount != 0 || report.VerifiedManifestCount != 1 {
		t.Fatalf("doctor after build retry: report=%#v err=%v", report, doctorErr)
	}
}

func TestPackGenerationRemovalRequiresSurvivingByteCompleteCopies(t *testing.T) {
	root := t.TempDir()
	value := bytes.Repeat([]byte("surviving-pack-proof\n"), 1024)
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if err := os.Remove(fold.NewObjectStore(root).ObjectPath(ref.SHA256)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err := storage.Collect(context.Background(), storage.GCOptions{
		StoreDir: root, Apply: true, KeepPackGenerations: 2,
		AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
			return AuthorizeGenerationRemoval(ctx, root, candidate)
		},
	})
	if err != nil {
		t.Fatalf("verified pack cleanup: %v", err)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("verified pack cleanup result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "packs", first.Generation)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("byte-redundant old generation remains: %v", err)
	}
}

func TestPackGenerationRemovalKeepsLastGoodPackWhenCurrentBytesAreCorrupt(t *testing.T) {
	root := t.TempDir()
	value := bytes.Repeat([]byte("last-good-pack\n"), 1024)
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if err := os.Remove(fold.NewObjectStore(root).ObjectPath(ref.SHA256)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	current, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packs", current.Generation, "pack-000001.pack"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = storage.Collect(context.Background(), storage.GCOptions{
		StoreDir: root, Apply: true, KeepPackGenerations: 2,
		AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
			return AuthorizeGenerationRemoval(ctx, root, candidate)
		},
	})
	if err == nil {
		t.Fatal("corrupt surviving bytes authorized deletion of the last good pack")
	}
	if _, statErr := os.Stat(filepath.Join(root, "packs", first.Generation)); statErr != nil {
		t.Fatalf("last good pack generation was removed: %v", statErr)
	}
}

func TestPackGenerationRemovalPreservesUnknownNonemptyCandidateContent(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("pack-ownership-proof"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "packs", first.Generation, "foreign-evidence")
	if err := os.WriteFile(foreign, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Dir(foreign), old, old); err != nil {
		t.Fatal(err)
	}
	_, err = storage.Collect(context.Background(), storage.GCOptions{
		StoreDir: root, Apply: true, KeepPackGenerations: 2,
		AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
			return AuthorizeGenerationRemoval(ctx, root, candidate)
		},
	})
	if err == nil {
		t.Fatal("unknown nonempty pack content was treated as generation ownership proof")
	}
	if got, statErr := os.ReadFile(foreign); statErr != nil || string(got) != "must survive" {
		t.Fatalf("unknown pack evidence changed: got=%q err=%v", got, statErr)
	}
}

func TestReadPublishedMarkerRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("strict published marker"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "packs", result.Generation, publishedMarkerFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatal(err)
	}
	marker["foreign_evidence"] = "must not become pack ownership"
	data, err = json.MarshalIndent(marker, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := readPublishedMarker(filepath.Dir(path)); err == nil {
		t.Fatal("published marker with an unknown field was accepted")
	}
}

func TestBuildBindsPublicationChainToStableStoreIdentity(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("stable pack store identity"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity, err := readPackStoreIdentity(filepath.Join(root, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	firstMarker, err := readPublishedMarker(filepath.Join(root, "packs", first.Generation))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := readPackStoreIdentity(filepath.Join(root, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	head, secondMarker, err := readPublicationHead(filepath.Join(root, "packs"))
	if err != nil {
		t.Fatal(err)
	}
	if firstIdentity != secondIdentity || firstMarker.StoreID != firstIdentity.StoreID || secondMarker.StoreID != firstIdentity.StoreID || head.StoreID != firstIdentity.StoreID || head.Generation != second.Generation {
		t.Fatalf("publication store identity drifted: first=%#v second=%#v first_marker=%#v second_marker=%#v head=%#v", firstIdentity, secondIdentity, firstMarker, secondMarker, head)
	}
}

func TestPackGenerationRemovalRejectsGenerationFromAnotherStore(t *testing.T) {
	source := t.TempDir()
	sourceRefs := putObjects(t, source, []byte("source store generation"))
	writeManifest(t, source, "source-session", sourceRefs)
	sourceBuild, err := Build(context.Background(), source, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	targetRefs := putObjects(t, target, []byte("target store generation"))
	writeManifest(t, target, "target-session", targetRefs)
	targetBuild, err := Build(context.Background(), target, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sourceBuild.Generation == targetBuild.Generation {
		t.Fatal("test requires distinct pack generation names")
	}
	foreign := filepath.Join(target, "packs", sourceBuild.Generation)
	if err := os.CopyFS(foreign, os.DirFS(filepath.Join(source, "packs", sourceBuild.Generation))); err != nil {
		t.Fatal(err)
	}

	_, err = storage.Collect(context.Background(), storage.GCOptions{
		StoreDir: target, Apply: true, KeepPackGenerations: 1,
		AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
			return AuthorizeGenerationRemoval(ctx, target, candidate)
		},
	})
	if err == nil {
		t.Fatal("another store's published generation was authorized for deletion")
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("foreign store generation was removed: %v", err)
	}
}

func TestPackGenerationRemovalPreservesLegacyUnboundCandidate(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("legacy candidate must fail closed"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "packs", first.Generation)
	marker, err := readPublishedMarker(directory)
	if err != nil {
		t.Fatal(err)
	}
	marker.Version = legacyPublishedVersion
	marker.Kind = legacyPublishedKind
	marker.StoreID = ""
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, publishedMarkerFilename), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = storage.Collect(context.Background(), storage.GCOptions{
		StoreDir: root, Apply: true, KeepPackGenerations: 1,
		AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
			return AuthorizeGenerationRemoval(ctx, root, candidate)
		},
	})
	if err == nil {
		t.Fatal("legacy unbound pack generation was authorized for deletion")
	}
	if _, err := os.Lstat(directory); err != nil {
		t.Fatalf("legacy unbound generation was removed: %v", err)
	}
}

func TestPackGenerationRemovalPreservesReplacementAfterFinalProof(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("pack staging replacement fence"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(root, "packs", first.Generation)
	saved := filepath.Join(root, "saved-proved-generation")
	replacement := filepath.Join(candidate, "foreign-evidence")
	swapped := false

	_, err = storage.Collect(context.Background(), storage.GCOptions{
		StoreDir: root, Apply: true, KeepPackGenerations: 1,
		AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
			return AuthorizeGenerationRemoval(ctx, root, candidate)
		},
		BeforePackStage: func(current storage.GCCandidate) error {
			if current.Path != candidate || swapped {
				return nil
			}
			swapped = true
			if err := os.Rename(candidate, saved); err != nil {
				return err
			}
			if err := os.MkdirAll(candidate, 0o700); err != nil {
				return err
			}
			return os.WriteFile(replacement, []byte("replacement must survive"), 0o600)
		},
	})
	if err == nil {
		t.Fatal("replacement tree passed staged exact revalidation")
	}
	if !swapped {
		t.Fatal("pack replacement hook did not run")
	}
	if got, err := os.ReadFile(replacement); err != nil || string(got) != "replacement must survive" {
		t.Fatalf("replacement evidence was not restored intact: got=%q err=%v", got, err)
	}
	if _, err := os.Lstat(saved); err != nil {
		t.Fatalf("original proved generation was lost: %v", err)
	}
}

func TestBuildBudgetRejectsBeforeCreatingCandidateGeneration(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("budgeted-pack-object"))
	writeManifest(t, root, "session", refs)
	checker := rejectingChecker{}
	if _, err := Build(context.Background(), root, BuildOptions{Budget: &checker}); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("Build error = %v, want storage budget rejection", err)
	}
	if checker.Calls != 1 {
		t.Fatalf("budget checks = %d, want 1", checker.Calls)
	}
	if _, err := os.Stat(filepath.Join(root, "packs")); !os.IsNotExist(err) {
		t.Fatalf("candidate packs directory exists after preflight rejection: %v", err)
	}
}

func TestBuildReportsStorageAccounting(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("accounted-pack"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Storage == nil || result.Storage.Budget.ProjectedPeakBytes <= result.Storage.Budget.CurrentPhysicalBytes || result.Storage.After.Packs.ApparentBytes == 0 {
		t.Fatalf("pack storage accounting is incomplete: %#v", result.Storage)
	}
}

func TestResolverAndDoctorDetectPackCorruption(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("protected"), 10000))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	index := loadCurrentIndex(t, root)
	block := index.Objects[0].Blocks[0]
	packPath := filepath.Join(root, "packs", index.Generation, block.Pack)
	file, err := os.OpenFile(packPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open pack: %v", err)
	}
	if _, err := file.WriteAt([]byte{0xff}, block.PackOffset); err != nil {
		_ = file.Close()
		t.Fatalf("corrupt pack: %v", err)
	}
	_ = file.Close()

	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer resolver.Close()
	if _, err := resolver.ReadAt(context.Background(), refs[0], make([]byte, 16), 0); err == nil {
		t.Fatal("ReadAt should reject a corrupt pack block")
	}
	report, err := Doctor(context.Background(), root)
	if err != nil {
		t.Fatalf("Doctor returned error: %v", err)
	}
	if report.IssueCount == 0 {
		t.Fatalf("Doctor did not report corruption: %#v", report)
	}
}

func TestBuildIncludesObjectsReferencedByGenerationManifests(t *testing.T) {
	root := t.TempDir()
	data := []byte("generation-only-object")
	store := fold.NewObjectStore(root)
	ref, _, err := store.Put(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SyncPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := fold.Manifest{
		Version: fold.ManifestVersion, Kind: fold.ManifestKind,
		Session: fold.ManifestSession{ID: "session", RolloutPath: "session.jsonl", Archived: true},
		Source:  fold.ManifestSource{Bytes: int64(len(data)), SHA256: ref.SHA256},
		Parts:   []fold.Part{{Kind: fold.PartResidual, Object: ref}},
	}
	manifestPath := filepath.Join(root, "manifests", "generations", "session", "2.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.ObjectCount != 1 {
		t.Fatalf("object count = %d, want 1", result.ObjectCount)
	}
}

func putObjects(t *testing.T, root string, values ...[]byte) []fold.ObjectRef {
	t.Helper()
	store := fold.NewObjectStore(root)
	refs := make([]fold.ObjectRef, 0, len(values))
	for _, value := range values {
		ref, _, err := store.Put(value, true)
		if err != nil {
			t.Fatalf("Put returned error: %v", err)
		}
		refs = append(refs, ref)
	}
	if err := store.SyncPending(context.Background()); err != nil {
		t.Fatalf("SyncPending returned error: %v", err)
	}
	return refs
}

func writeManifest(t *testing.T, root string, sessionID string, refs []fold.ObjectRef) {
	t.Helper()
	manifest := fold.Manifest{
		Version: fold.ManifestVersion,
		Kind:    fold.ManifestKind,
		Session: fold.ManifestSession{ID: sessionID, RolloutPath: filepath.Join(root, sessionID+".jsonl")},
		Parts:   make([]fold.Part, 0, len(refs)),
	}
	hasher := sha256.New()
	store := fold.NewObjectStore(root)
	for _, ref := range refs {
		manifest.Source.Bytes += ref.RawBytes
		manifest.Parts = append(manifest.Parts, fold.Part{Kind: fold.PartResidual, Object: ref})
		value, err := store.Read(ref)
		if err != nil {
			t.Fatalf("read manifest object: %v", err)
		}
		_, _ = hasher.Write(value)
	}
	manifest.Source.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "manifests"), 0o755); err != nil {
		t.Fatalf("create manifests: %v", err)
	}
	if err := os.WriteFile(fold.ManifestPath(root, sessionID), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func persistManagedManifestFixture(t *testing.T, root string, sessionID string, source []byte) vfs.SessionState {
	t.Helper()
	manifest, err := fold.LoadManifest(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Source.Bytes != int64(len(source)) || manifest.Source.SHA256 != digestBytes(source) {
		t.Fatalf("managed fixture source does not match manifest: source=%#v bytes=%d sha=%s", manifest.Source, len(source), digestBytes(source))
	}
	if err := os.WriteFile(manifest.Session.RolloutPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	nativeSnapshot := filepath.Join(root, "fs", "snapshots", sessionID, "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(nativeSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, nativeSnapshot); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, sessionID), Manifest: manifest,
		Reader: resolver, NativeSnapshot: vfs.NativeFile{Path: nativeSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err != nil {
		t.Fatalf("publish managed state fixture: %v", err)
	}
	return session.State()
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func loadCurrentIndex(t *testing.T, root string) Index {
	t.Helper()
	current, err := os.ReadFile(filepath.Join(root, "packs", "CURRENT"))
	if err != nil {
		t.Fatalf("read CURRENT: %v", err)
	}
	directory := filepath.Join(root, "packs", string(bytes.TrimSpace(current)))
	if v3, err := openIndexV3(directory); err == nil {
		defer v3.close()
		index := Index{Version: v3.meta.Version, Kind: v3.meta.Kind, Generation: v3.meta.Generation, CreatedAt: v3.meta.CreatedAt, BlockBytes: v3.meta.BlockBytes, Objects: make([]Object, 0, v3.meta.ObjectCount)}
		for position := int64(0); position < v3.meta.ObjectCount; position++ {
			object, err := v3.objectAt(position)
			if err != nil {
				t.Fatalf("read v3 object %d: %v", position, err)
			}
			index.Objects = append(index.Objects, object)
		}
		return index
	}
	data, err := os.ReadFile(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	return index
}

type rejectingChecker struct {
	Calls int
}

func (c *rejectingChecker) Check(context.Context, storage.Projection) (storage.Assessment, error) {
	c.Calls++
	return storage.Assessment{}, storage.ErrBudgetExceeded
}
