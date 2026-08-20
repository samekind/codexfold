package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testPackRemovalGuard struct {
	revalidate       func() error
	revalidateStaged func(GCCandidate) error
}

func (g testPackRemovalGuard) Revalidate(context.Context) error {
	if g.revalidate != nil {
		return g.revalidate()
	}
	return nil
}
func (g testPackRemovalGuard) RevalidateStaged(_ context.Context, candidate GCCandidate) error {
	if g.revalidateStaged != nil {
		return g.revalidateStaged(candidate)
	}
	return nil
}
func (testPackRemovalGuard) Close() error { return nil }

type testExactRemovalGuard struct {
	proof      ExactRemovalProof
	revalidate func() error
}

func (g testExactRemovalGuard) Proof() ExactRemovalProof { return g.proof }
func (g testExactRemovalGuard) Revalidate(context.Context) error {
	if g.revalidate != nil {
		return g.revalidate()
	}
	return nil
}
func (testExactRemovalGuard) Close() error { return nil }

func TestCollectBoundsGenerationsRetiredStateAndTemporaryFiles(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	now := time.Unix(2_000_000, 0)
	old := now.Add(-2 * time.Hour)

	writeBytesFile(t, filepath.Join(store, "packs", "CURRENT"), []byte("gen-3\n"))
	for _, generation := range []string{"gen-0", "gen-1", "gen-2", "gen-3"} {
		path := writeSizedFile(t, filepath.Join(store, "packs", generation, "pack-000001.pack"), 16)
		setModTime(t, path, old.Add(time.Duration(generation[len(generation)-1]-'0')*time.Minute))
		writeJSONFile(t, filepath.Join(store, "packs", generation, "index.json"), map[string]any{"generation": generation})
	}
	leased, err := AcquireLease(filepath.Join(store, "packs", "gen-0", "leases"), "resolver")
	if err != nil {
		t.Fatal(err)
	}

	manifestRoot := filepath.Join(store, "manifests", "generations", "session")
	for generation := 1; generation <= 3; generation++ {
		writeJSONFile(t, filepath.Join(manifestRoot, string(rune('0'+generation))+".json"), manifestFixture("session", filepath.Join(store, "native.jsonl"), int64(generation*10)))
	}
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	currentDelta := writeSizedFile(t, filepath.Join(sessionDir, "delta-00000000000000000003.jsonl"), 3)
	oldDelta := writeSizedFile(t, filepath.Join(sessionDir, "delta-00000000000000000001.jsonl"), 7)
	oldBacking := writeSizedFile(t, filepath.Join(sessionDir, "backing-00000000000000000002.jsonl"), 9)
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), stateFixture(t,
		"session", filepath.Join(manifestRoot, "3.json"), 30, currentDelta, "", "",
	))

	for index := 1; index <= 3; index++ {
		directory := filepath.Join(store, "fs", "retired", "retired-"+string(rune('0'+index)))
		writeJSONFile(t, filepath.Join(directory, "state.json"), map[string]any{"session_id": "session"})
		setModTime(t, directory, old.Add(time.Duration(index)*time.Minute))
	}
	oldTemp := writeSizedFile(t, filepath.Join(store, "fs", "sessions", "session", ".backing-abandoned.tmp"), 11)
	recentTemp := writeSizedFile(t, filepath.Join(store, "fs", "sessions", "session", ".state-recent.tmp"), 13)
	setModTime(t, oldTemp, old)
	setModTime(t, recentTemp, now.Add(-10*time.Minute))
	setModTime(t, oldDelta, old)
	setModTime(t, oldBacking, old)

	options := GCOptions{
		StoreDir: store, TemporaryGrace: time.Hour, Now: func() time.Time { return now },
		KeepPackGenerations: 2, KeepManifestGenerations: 2, KeepRetiredPerSession: 1,
		AuthorizePackGenerationRemoval: func(context.Context, GCCandidate) (PackGenerationRemovalGuard, error) {
			return testPackRemovalGuard{}, nil
		},
	}
	dry, err := Collect(context.Background(), options)
	if err != nil {
		t.Fatalf("Collect dry-run: %v", err)
	}
	if !dry.DryRun || dry.CandidateCount != 7 || dry.RemovedCount != 0 || dry.ProjectedReclaimableBytes <= 0 || dry.ActualReclaimedBytes != 0 {
		t.Fatalf("unexpected dry-run result: %#v", dry)
	}
	for _, path := range []string{filepath.Join(store, "packs", "gen-1"), oldDelta, oldBacking, oldTemp, filepath.Join(manifestRoot, "1.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry-run removed %s: %v", path, err)
		}
	}

	options.Apply = true
	applied, err := Collect(context.Background(), options)
	if err != nil {
		t.Fatalf("Collect apply: %v", err)
	}
	if applied.RemovedCount != 1 || applied.RetainedUnprovedCount != dry.CandidateCount-1 || applied.ActualReclaimedBytes <= 0 {
		t.Fatalf("unexpected apply result: %#v", applied)
	}
	for _, path := range []string{
		filepath.Join(store, "packs", "gen-3"), filepath.Join(store, "packs", "gen-2"), filepath.Join(store, "packs", "gen-0"),
		currentDelta, oldDelta, oldBacking, oldTemp,
		filepath.Join(manifestRoot, "1.json"), filepath.Join(manifestRoot, "2.json"), filepath.Join(manifestRoot, "3.json"),
		filepath.Join(store, "fs", "retired", "retired-1"), filepath.Join(store, "fs", "retired", "retired-2"), filepath.Join(store, "fs", "retired", "retired-3"), recentTemp,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained path missing %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(store, "packs", packGCStagingDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pack generation staging was not cleaned: %v", err)
	}

	if err := leased.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Collect(context.Background(), options)
	if err != nil {
		t.Fatalf("Collect after lease close: %v", err)
	}
	if second.RemovedCount != 1 || second.RetainedUnprovedCount == 0 {
		t.Fatalf("closed leased generation was not collected: %#v", second)
	}
	third, err := Collect(context.Background(), options)
	if err != nil || third.RemovedCount != 0 {
		t.Fatalf("repeated Collect is not idempotent: %#v err=%v", third, err)
	}
}

func TestCollectKeepsSoleRecoveryStateAndJournalOwnedFiles(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	now := time.Unix(3_000_000, 0)
	manifest := filepath.Join(store, "manifests", "generations", "session", "1.json")
	writeJSONFile(t, manifest, manifestFixture("session", filepath.Join(store, "native.jsonl"), 5))
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	delta := writeSizedFile(t, filepath.Join(sessionDir, "delta.jsonl"), 5)
	scratch := writeSizedFile(t, filepath.Join(sessionDir, ".compact-00000000000000000001.jsonl"), 5)
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), stateFixture(t, "session", manifest, 0, delta, "", ""))
	writeJSONLine(t, filepath.Join(sessionDir, "journal.jsonl"), map[string]any{
		"operation_id": "compact-1", "phase": "prepared", "native": map[string]any{"path": scratch},
	})
	writeJSONFile(t, filepath.Join(store, "fs", "retired", "only", "state.json"), map[string]any{"session_id": "session"})
	writeSizedFile(t, filepath.Join(store, "packs", "gen-only", "pack-000001.pack"), 5)
	setModTime(t, scratch, now.Add(-24*time.Hour))

	result, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true, TemporaryGrace: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.RemovedCount != 0 {
		t.Fatalf("sole recovery state was removed: %#v", result)
	}
	for _, path := range []string{manifest, delta, scratch, filepath.Join(store, "fs", "retired", "only"), filepath.Join(store, "packs", "gen-only")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected path missing %s: %v", path, err)
		}
	}
}

func TestCollectKeepsUnprovedAbandonedManifestAfterTerminalJournal(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	manifestRoot := filepath.Join(store, "manifests", "generations", "session")
	for generation := 1; generation <= 3; generation++ {
		writeJSONFile(t, filepath.Join(manifestRoot, string(rune('0'+generation))+".json"), manifestFixture("session", filepath.Join(store, "native.jsonl"), int64(generation)))
	}
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	delta := writeSizedFile(t, filepath.Join(sessionDir, "delta-00000000000000000002.jsonl"), 2)
	state := stateFixture(t, "session", filepath.Join(manifestRoot, "2.json"), 2, delta, "", "")
	state["generation"] = 2
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), state)
	writeJSONLine(t, filepath.Join(sessionDir, "journal.jsonl"), map[string]any{"operation_id": "compact-2", "phase": "prepared"})

	blocked, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.RemovedCount != 0 {
		t.Fatalf("pending compaction allowed manifest cleanup: %#v", blocked)
	}
	writeJSONLine(t, filepath.Join(sessionDir, "journal.jsonl"), map[string]any{"operation_id": "compact-2", "phase": "rolled-back"})
	collected, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if collected.RemovedCount != 0 || collected.RetainedUnprovedCount != 1 {
		t.Fatalf("unproved abandoned future manifest was not retained: %#v", collected)
	}
	for _, name := range []string{"1.json", "2.json", "3.json"} {
		if _, err := os.Stat(filepath.Join(manifestRoot, name)); err != nil {
			t.Fatalf("unproved manifest missing %s: %v", name, err)
		}
	}
}

func TestCollectReportsZeroPhysicalReclamationForRemainingHardLink(t *testing.T) {
	store := t.TempDir()
	keep := writeSizedFile(t, filepath.Join(store, "keep.bin"), 4096)
	temporary := filepath.Join(store, ".backing-abandoned.tmp")
	if err := os.Link(keep, temporary); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	setModTime(t, temporary, old)
	result, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true, TemporaryGrace: time.Hour, Now: time.Now})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.RemovedCount != 0 || result.RetainedUnprovedCount != 1 || result.ProjectedReclaimableBytes != 0 || result.ActualReclaimedBytes != 0 {
		t.Fatalf("hard-link reclamation was overstated: %#v", result)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("retained hard link missing: %v", err)
	}
	if _, err := os.Stat(temporary); err != nil {
		t.Fatalf("unproved temporary hard link was removed: %v", err)
	}
}

func TestCollectRetainsUnknownNonemptyTemporaryAndRetiredState(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	now := time.Unix(4_000_000, 0)
	old := now.Add(-24 * time.Hour)
	temporary := writeSizedFile(t, filepath.Join(store, ".backing-unknown.tmp"), 17)
	setModTime(t, temporary, old)
	for _, name := range []string{"newer", "older"} {
		directory := filepath.Join(store, "fs", "retired", name)
		writeJSONFile(t, filepath.Join(directory, "state.json"), map[string]any{"session_id": "session"})
		writeBytesFile(t, filepath.Join(directory, "unknown.jsonl"), []byte("unknown nonempty session bytes\n"))
	}
	setModTime(t, filepath.Join(store, "fs", "retired", "newer"), old.Add(time.Hour))
	setModTime(t, filepath.Join(store, "fs", "retired", "older"), old)

	result, err := Collect(context.Background(), GCOptions{
		StoreDir: store, Apply: true, TemporaryGrace: time.Hour, KeepRetiredPerSession: 1,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.CandidateCount != 2 || result.RemovedCount != 0 || result.RetainedUnprovedCount != 2 {
		t.Fatalf("unknown nonempty data was not reported and retained: %#v", result)
	}
	for _, path := range []string{temporary, filepath.Join(store, "fs", "retired", "older", "unknown.jsonl")} {
		if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
			t.Fatalf("unknown nonempty candidate changed %s: bytes=%d err=%v", path, len(data), err)
		}
	}
}

func TestCollectDeletesNonPackCandidateOnlyWithDurableExactProof(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	now := time.Unix(5_000_000, 0)
	temporary := writeSizedFile(t, filepath.Join(store, ".backing-proved.tmp"), 23)
	setModTime(t, temporary, now.Add(-2*time.Hour))
	dry, err := Collect(context.Background(), GCOptions{
		StoreDir: store, TemporaryGrace: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil || len(dry.Candidates) != 1 {
		t.Fatalf("discover exact-proof candidate: %#v err=%v", dry, err)
	}
	proof, err := CaptureExactRemovalProof(context.Background(), dry.Candidates[0], "cleanup-operation-1")
	if err != nil {
		t.Fatalf("CaptureExactRemovalProof: %v", err)
	}
	proofPath := filepath.Join(store, "gc-proofs", "cleanup-operation-1.json")
	writeJSONFile(t, proofPath, proof)

	loadProof := func() (ExactRemovalProof, error) {
		data, err := os.ReadFile(proofPath)
		if err != nil {
			return ExactRemovalProof{}, err
		}
		var current ExactRemovalProof
		if err := json.Unmarshal(data, &current); err != nil {
			return ExactRemovalProof{}, err
		}
		return current, nil
	}
	result, err := Collect(context.Background(), GCOptions{
		StoreDir: store, Apply: true, TemporaryGrace: time.Hour, Now: func() time.Time { return now },
		AuthorizeExactRemoval: func(_ context.Context, candidate GCCandidate) (ExactRemovalGuard, error) {
			if candidate.Path != temporary {
				return nil, nil
			}
			persisted, err := loadProof()
			if err != nil {
				return nil, err
			}
			return testExactRemovalGuard{proof: persisted, revalidate: func() error {
				refreshed, err := loadProof()
				if err != nil {
					return err
				}
				if refreshed != persisted {
					return errors.New("durable cleanup proof changed")
				}
				return nil
			}}, nil
		},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.RemovedCount != 1 || result.RetainedUnprovedCount != 0 {
		t.Fatalf("exactly proved candidate was not removed: %#v", result)
	}
	if _, err := os.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proved temporary remains: %v", err)
	}
	if _, err := os.Stat(proofPath); err != nil {
		t.Fatalf("durable cleanup proof was removed with its candidate: %v", err)
	}
}

func TestCollectRefusesExactProofWhenCandidateBytesChange(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	now := time.Unix(6_000_000, 0)
	temporary := filepath.Join(store, ".backing-proved.tmp")
	writeBytesFile(t, temporary, []byte("original bytes"))
	setModTime(t, temporary, now.Add(-2*time.Hour))
	proof := exactRemovalProofFixture(t, temporary, CandidateTemporary, "cleanup-operation-2")

	result, err := Collect(context.Background(), GCOptions{
		StoreDir: store, Apply: true, TemporaryGrace: time.Hour, Now: func() time.Time { return now },
		AuthorizeExactRemoval: func(context.Context, GCCandidate) (ExactRemovalGuard, error) {
			return testExactRemovalGuard{proof: proof}, nil
		},
		BeforeRemove: func(candidate GCCandidate) error {
			if candidate.Path == temporary {
				if err := os.WriteFile(temporary, []byte("tampered bytes"), 0o600); err != nil {
					return err
				}
				return os.Chtimes(temporary, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
			}
			return nil
		},
	})
	if err == nil {
		t.Fatalf("changed candidate bytes were removed: %#v", result)
	}
	if data, statErr := os.ReadFile(temporary); statErr != nil || string(data) != "tampered bytes" {
		t.Fatalf("changed candidate evidence was not retained: bytes=%q err=%v", data, statErr)
	}
}

func TestCollectRechecksPackCurrentImmediatelyBeforeRemoval(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	writeBytesFile(t, filepath.Join(store, "packs", "CURRENT"), []byte("gen-3\n"))
	for _, generation := range []string{"gen-1", "gen-2", "gen-3"} {
		writeSizedFile(t, filepath.Join(store, "packs", generation, "pack-000001.pack"), 8)
		writeJSONFile(t, filepath.Join(store, "packs", generation, "index.json"), map[string]any{"generation": generation})
	}
	candidate := filepath.Join(store, "packs", "gen-1")
	changed := false
	result, err := Collect(context.Background(), GCOptions{
		StoreDir: store, Apply: true, KeepPackGenerations: 2,
		AuthorizePackGenerationRemoval: func(context.Context, GCCandidate) (PackGenerationRemovalGuard, error) {
			return testPackRemovalGuard{}, nil
		},
		BeforeRemove: func(current GCCandidate) error {
			if current.Path == candidate && !changed {
				changed = true
				writeBytesFile(t, filepath.Join(store, "packs", "CURRENT"), []byte("gen-1\n"))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !changed || result.RemovedCount != 0 {
		t.Fatalf("candidate promoted to CURRENT was removed: %#v", result)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("new current generation missing: %v", err)
	}
}

func TestCollectStagesPackGenerationAndPreservesReplacement(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	writeBytesFile(t, filepath.Join(store, "packs", "CURRENT"), []byte("gen-2\n"))
	for _, generation := range []string{"gen-1", "gen-2"} {
		writeSizedFile(t, filepath.Join(store, "packs", generation, "pack-000001.pack"), 8)
		writeJSONFile(t, filepath.Join(store, "packs", generation, "index.json"), map[string]any{"generation": generation})
	}
	candidatePath := filepath.Join(store, "packs", "gen-1")
	expectedFiles, expectedBytes, expectedDigest, err := exactCandidateTreeIdentity(context.Background(), candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(store, "saved-proved-generation")
	replacement := filepath.Join(candidatePath, "foreign-evidence")
	swapped := false

	_, err = Collect(context.Background(), GCOptions{
		StoreDir: store, Apply: true, KeepPackGenerations: 1,
		AuthorizePackGenerationRemoval: func(context.Context, GCCandidate) (PackGenerationRemovalGuard, error) {
			return testPackRemovalGuard{revalidateStaged: func(staged GCCandidate) error {
				files, bytes, digest, err := exactCandidateTreeIdentity(context.Background(), staged.Path)
				if err != nil {
					return err
				}
				if files != expectedFiles || bytes != expectedBytes || digest != expectedDigest {
					return errors.New("staged candidate differs from the proved generation")
				}
				return nil
			}}, nil
		},
		BeforePackStage: func(candidate GCCandidate) error {
			if candidate.Path != candidatePath || swapped {
				return nil
			}
			swapped = true
			if err := os.Rename(candidatePath, saved); err != nil {
				return err
			}
			if err := os.MkdirAll(candidatePath, 0o700); err != nil {
				return err
			}
			return os.WriteFile(replacement, []byte("replacement must survive"), 0o600)
		},
	})
	if err == nil {
		t.Fatal("replacement tree passed staged exact revalidation")
	}
	if !swapped {
		t.Fatal("pack staging race hook did not run")
	}
	if got, err := os.ReadFile(replacement); err != nil || string(got) != "replacement must survive" {
		t.Fatalf("replacement was not restored intact: got=%q err=%v", got, err)
	}
	if _, err := os.Lstat(saved); err != nil {
		t.Fatalf("proved generation was lost: %v", err)
	}
}

func TestCollectDoesNotDeletePackGenerationWithoutByteProof(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	writeBytesFile(t, filepath.Join(store, "packs", "CURRENT"), []byte("gen-3\n"))
	for _, generation := range []string{"gen-1", "gen-2", "gen-3"} {
		writeSizedFile(t, filepath.Join(store, "packs", generation, "pack-000001.pack"), 8)
		writeJSONFile(t, filepath.Join(store, "packs", generation, "index.json"), map[string]any{"generation": generation})
	}
	candidate := filepath.Join(store, "packs", "gen-1")
	result, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true, KeepPackGenerations: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedCount != 0 {
		t.Fatalf("pack generation was removed without an external byte proof: %#v", result)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("unproved pack candidate missing: %v", err)
	}
}

func TestCollectRefusesDeletionWhenManagedStateBecomesCorrupt(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	manifestRoot := filepath.Join(store, "manifests", "generations", "session")
	for generation := 1; generation <= 3; generation++ {
		writeJSONFile(t, filepath.Join(manifestRoot, string(rune('0'+generation))+".json"), manifestFixture("session", filepath.Join(store, "native.jsonl"), int64(generation)))
	}
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	delta := writeSizedFile(t, filepath.Join(sessionDir, "delta.jsonl"), 1)
	statePath := filepath.Join(sessionDir, "state.json")
	writeJSONFile(t, statePath, stateFixture(t, "session", filepath.Join(manifestRoot, "3.json"), 3, delta, "", ""))
	candidate := filepath.Join(manifestRoot, "1.json")
	proof := exactRemovalProofFixture(t, candidate, CandidateManifestGeneration, "manifest-cleanup-1")
	result, err := Collect(context.Background(), GCOptions{
		StoreDir: store, Apply: true, KeepManifestGenerations: 2,
		AuthorizeExactRemoval: func(_ context.Context, current GCCandidate) (ExactRemovalGuard, error) {
			if current.Path != candidate {
				return nil, nil
			}
			return testExactRemovalGuard{proof: proof}, nil
		},
		BeforeRemove: func(current GCCandidate) error {
			if current.Path == candidate {
				return os.WriteFile(statePath, []byte("{broken"), 0o600)
			}
			return nil
		},
	})
	if err == nil {
		t.Fatalf("corrupt managed state allowed cleanup: %#v", result)
	}
	if _, statErr := os.Stat(candidate); statErr != nil {
		t.Fatalf("candidate removed without a complete state proof: %v", statErr)
	}
}

func setModTime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func writeMinimalState(t *testing.T, path string, sessionID string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"session_id": sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func exactRemovalProofFixture(t *testing.T, path string, kind CandidateKind, operationID string) ExactRemovalProof {
	t.Helper()
	files, apparentBytes, digest, err := exactCandidateTreeIdentity(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return ExactRemovalProof{
		OperationID: operationID, Kind: kind, Path: cleanAbsolutePath(path),
		Files: files, ApparentBytes: apparentBytes, TreeSHA256: digest,
	}
}
