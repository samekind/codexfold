package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"time"
)

// ManagedSessionReference is the persisted relationship that must remain
// provable before storage shared by a managed session can be reclaimed.
type ManagedSessionReference struct {
	StoreDir       string
	SessionID      string
	ManifestPath   string
	ManifestSHA256 string
	BaseBytes      int64
	BaseSHA256     string
}

// ManagedSessionDeletionGuard keeps every managed session's writer lock held
// while shared storage is being reclaimed.
type ManagedSessionDeletionGuard struct {
	References []ManagedSessionReference
	files      []*os.File
	sessions   managedSessionsStamp
	proofFiles map[string]os.FileInfo
	closed     bool
}

// ErrManagedSessionBusy is ordinary live I/O contention, not corrupt storage.
var ErrManagedSessionBusy = errors.New("managed session is busy")

func ManagedSessionReferences(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
	s, exists, err := prepareScanner(ctx, Options{StoreDir: storeDir})
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return managedSessionReferences(s), nil
}

func (s *scanner) validateManagedReferences() error {
	for sessionID, state := range s.managedStates {
		if state.Version != 2 || !validSHA256(state.ManifestSHA256) {
			return fmt.Errorf("managed session %s has no exact manifest identity", sessionID)
		}
		manifestPath, err := cleanPathWithin(filepath.Join(s.store, "manifests"), state.ManifestPath)
		if err != nil {
			return fmt.Errorf("managed session %s manifest escapes the canonical manifest store: %w", sessionID, err)
		}
		reference := ManagedSessionReference{
			StoreDir: s.store, SessionID: sessionID, ManifestPath: manifestPath, ManifestSHA256: state.ManifestSHA256,
			BaseBytes: state.BaseBytes, BaseSHA256: state.BaseSHA256,
		}
		if err := ValidateManagedSessionReference(reference); err != nil {
			return fmt.Errorf("load current manifest for managed session %s: %w", sessionID, err)
		}
		for label, path := range map[string]string{"delta": state.DeltaPath, "backing": state.BackingPath} {
			if path == "" {
				continue
			}
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("stat managed session %s %s: %w", sessionID, label, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("managed session %s %s is not a regular file", sessionID, label)
			}
		}
	}
	return nil
}

// ValidateManagedSessionReference verifies the exact on-disk manifest bytes,
// not merely the decoded session and source fields. Destructive callers must
// reject legacy state that cannot name one exact manifest version.
func ValidateManagedSessionReference(reference ManagedSessionReference) error {
	if reference.SessionID == "" || !validSHA256(reference.ManifestSHA256) || reference.BaseBytes < 0 || !validSHA256(reference.BaseSHA256) {
		return errors.New("managed session reference is incomplete")
	}
	if reference.StoreDir == "" {
		return errors.New("managed session reference has no store root")
	}
	manifest, digest, err := loadManagedManifestIdentity(reference.StoreDir, reference.ManifestPath)
	if err != nil {
		return err
	}
	if digest != reference.ManifestSHA256 {
		return errors.New("manifest bytes do not match managed session state")
	}
	if manifest.Session.ID != reference.SessionID || manifest.Source.Bytes != reference.BaseBytes || manifest.Source.SHA256 != reference.BaseSHA256 {
		return errors.New("managed session state does not match its current manifest")
	}
	return nil
}

func loadManagedManifestIdentity(storeDir string, path string) (manifestRecord, string, error) {
	manifestRoot := filepath.Join(filepath.Clean(storeDir), "manifests")
	path, err := cleanPathWithin(manifestRoot, path)
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("resolve manifest %s: %w", path, err)
	}
	relative, err := filepath.Rel(manifestRoot, path)
	if err != nil {
		return manifestRecord{}, "", err
	}
	storeRoot, err := os.OpenRoot(filepath.Clean(storeDir))
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("open managed store root: %w", err)
	}
	defer storeRoot.Close()
	root, err := storeRoot.OpenRoot("manifests")
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("open manifest root: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat(relative)
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("inspect manifest %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return manifestRecord{}, "", fmt.Errorf("manifest %s is not a regular file", path)
	}
	file, err := root.Open(relative)
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("open manifest %s: %w", path, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("stat opened manifest %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return manifestRecord{}, "", fmt.Errorf("manifest %s changed while it was opened", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return manifestRecord{}, "", fmt.Errorf("read manifest %s: %w", path, err)
	}
	var manifest manifestRecord
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifestRecord{}, "", fmt.Errorf("decode manifest %s: %w", path, err)
	}
	if err := validateManifestRecord(manifest); err != nil {
		return manifestRecord{}, "", fmt.Errorf("invalid manifest %s: %w", path, err)
	}
	digest := sha256.Sum256(data)
	return manifest, hex.EncodeToString(digest[:]), nil
}

// ManagedSessionDeletionProof reloads every managed state and current manifest
// and refuses a destructive operation while a session can still change them.
func ManagedSessionDeletionProof(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
	guard, err := AcquireManagedSessionDeletionGuard(ctx, storeDir)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	return guard.References, nil
}

// MetadataSettleWindow is how far a file's change time must trail the moment
// its metadata was read before that metadata can vouch for "nothing changed
// since".
//
// Filesystems stamp ctime from a coarse clock: one kernel tick on Linux, a
// whole second on HFS+, two on FAT. A second write landing in the same tick as
// the first is indistinguishable from it by metadata alone, however carefully
// the fields are compared. So a stamp is only trusted once every change it
// records is older than the coarsest of those granularities. Until then every
// check pays for the full proof, which is always correct; the cost is a few
// extra reloads right after a change, never a missed one.
var MetadataSettleWindow = 2 * time.Second

// MetadataSettled reports whether every change time in infos is far enough
// behind readAt, the moment reading began, that no later write could share its
// clock tick. Nil entries record an absent file and impose nothing. Platforms
// without a change-time field never settle.
func MetadataSettled(readAt time.Time, infos ...os.FileInfo) bool {
	cutoff := readAt.Add(-MetadataSettleWindow)
	for _, info := range infos {
		if info == nil {
			continue
		}
		changed, ok := fileChangeInstant(info)
		if !ok || !changed.Before(cutoff) {
			return false
		}
	}
	return true
}

// managedSessionsStamp identifies the managed session directory itself, which
// is enough to notice a session being added or removed: creating or removing
// `fs/sessions/<id>` necessarily modifies its parent.
//
// The entry names are part of the stamp because the directory's own metadata
// cannot be relied on to show it. On ext4 a directory's size stays at one block
// as entries come and go, and its timestamps share the coarse clock, so a
// session published in the same tick as the stamp leaves every field equal.
// Missing that publication would let reclamation delete objects the new
// session still references.
type managedSessionsStamp struct {
	valid bool
	info  os.FileInfo
	names []string
}

func (s managedSessionsStamp) same(other managedSessionsStamp) bool {
	return s.valid && other.valid && os.SameFile(s.info, other.info) && s.info.ModTime() == other.info.ModTime() && s.info.Size() == other.info.Size() && sameChangeTime(s.info, other.info) && slices.Equal(s.names, other.names)
}

func stampManagedSessions(storeDir string) managedSessionsStamp {
	directory := filepath.Join(filepath.Clean(storeDir), "fs", "sessions")
	info, err := os.Stat(directory)
	if err != nil {
		return managedSessionsStamp{}
	}
	if !info.IsDir() {
		return managedSessionsStamp{}
	}
	if _, supported := fileChangeTime(info); !supported {
		return managedSessionsStamp{}
	}
	names, err := readDirectoryNames(directory)
	if err != nil {
		return managedSessionsStamp{}
	}
	return managedSessionsStamp{
		valid: true,
		info:  info,
		names: names,
	}
}

func readDirectoryNames(directory string) ([]string, error) {
	file, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	names, err := file.Readdirnames(-1)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// reloadManagedSessionReferences is injectable so a test can count how often the
// expensive reload actually runs.
var reloadManagedSessionReferences = ManagedSessionReferences

// AcquireManagedSessionDeletionGuard reloads every managed state and current
// manifest, then holds writer locks so that proof stays valid until Close.
func AcquireManagedSessionDeletionGuard(ctx context.Context, storeDir string) (*ManagedSessionDeletionGuard, error) {
	s, exists, err := prepareScanner(ctx, Options{StoreDir: storeDir})
	if err != nil {
		return nil, err
	}
	if !exists {
		return &ManagedSessionDeletionGuard{}, nil
	}
	references := managedSessionReferences(s)
	guard := &ManagedSessionDeletionGuard{References: references}
	for _, reference := range references {
		sessionID := reference.SessionID
		if err := ctx.Err(); err != nil {
			_ = guard.Close()
			return nil, err
		}
		directory := filepath.Join(s.store, "fs", "sessions", sessionID)
		writer, err := acquireExclusiveFileLock(filepath.Join(directory, "writer.lease"))
		if err != nil {
			_ = guard.Close()
			return nil, fmt.Errorf("lock managed session %s writer: %w", sessionID, err)
		}
		guard.files = append(guard.files, writer)
		readerActive, err := treeHasActiveLease(filepath.Join(directory, "leases"))
		if err != nil {
			_ = guard.Close()
			return nil, fmt.Errorf("inspect managed session %s readers: %w", sessionID, err)
		}
		pending, err := journalPendingAt(directory)
		if err != nil {
			_ = guard.Close()
			return nil, fmt.Errorf("inspect managed session %s journal: %w", sessionID, err)
		}
		if readerActive {
			_ = guard.Close()
			return nil, fmt.Errorf("%w: managed session %s has an active reader", ErrManagedSessionBusy, sessionID)
		}
		if pending {
			_ = guard.Close()
			return nil, fmt.Errorf("managed session %s has unfinished recovery", sessionID)
		}
	}
	if _, err := guard.Refresh(ctx, storeDir); err != nil {
		_ = guard.Close()
		return nil, fmt.Errorf("refresh managed sessions after locking writers: %w", err)
	}
	return guard, nil
}

// Refresh re-proves that the managed session set has not moved underneath a
// long reclamation.
//
// Writer leases exclude cooperating writers. Directory and proof-file metadata
// additionally detect publication, replacement, and unexpected in-place edits.
// Unchanged metadata avoids decoding/hashing every manifest on every object.
func (g *ManagedSessionDeletionGuard) Refresh(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
	if g.closed {
		return nil, errors.New("managed session deletion guard is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Taken before any metadata is read: a cached stamp vouches for nothing
	// changing after this instant, so it is the one its settling is measured
	// against.
	readAt := time.Now()
	stamp := stampManagedSessions(storeDir)
	files, err := g.stampProofFiles(storeDir)
	if err != nil {
		return nil, err
	}
	if stamp.same(g.sessions) && sameProofFiles(files, g.proofFiles) {
		return g.References, nil
	}
	current, err := reloadManagedSessionReferences(ctx, storeDir)
	if err != nil {
		return nil, err
	}
	if len(current) != len(g.References) {
		return nil, errors.New("managed session set changed during deletion proof")
	}
	for index := range current {
		if current[index] != g.References[index] {
			return nil, fmt.Errorf("managed session %s changed during deletion proof", g.References[index].SessionID)
		}
	}
	// Never associate an old scan with a directory changed during that scan.
	after := stampManagedSessions(storeDir)
	if (stamp.valid || after.valid) && !stamp.same(after) {
		g.sessions = managedSessionsStamp{}
		return nil, errors.New("managed session directory changed during deletion proof refresh")
	}
	afterFiles, err := g.stampProofFiles(storeDir)
	if err != nil {
		return nil, err
	}
	if !sameProofFiles(files, afterFiles) {
		g.sessions = managedSessionsStamp{}
		return nil, errors.New("managed session proof files changed during refresh")
	}
	// The full reload above proved this result. Whether the next refresh may
	// skip that proof depends on the stamp being able to show a later change at
	// all, which a change still inside its clock tick cannot.
	if !after.valid || !MetadataSettled(readAt, after.info) || !MetadataSettled(readAt, proofFileInfos(afterFiles)...) {
		g.sessions = managedSessionsStamp{}
		g.proofFiles = nil
		return current, nil
	}
	g.sessions = after
	g.proofFiles = afterFiles
	return current, nil
}

func proofFileInfos(files map[string]os.FileInfo) []os.FileInfo {
	infos := make([]os.FileInfo, 0, len(files))
	for _, info := range files {
		infos = append(infos, info)
	}
	return infos
}

func (g *ManagedSessionDeletionGuard) stampProofFiles(storeDir string) (map[string]os.FileInfo, error) {
	files := make(map[string]os.FileInfo, len(g.References)*2)
	for _, ref := range g.References {
		for _, path := range []string{ref.ManifestPath, filepath.Join(storeDir, "fs", "sessions", ref.SessionID, "state.json")} {
			info, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("proof file is not regular: %s", path)
			}
			files[path] = info
		}
	}
	return files, nil
}

func sameProofFiles(a, b map[string]os.FileInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for path, info := range a {
		other, ok := b[path]
		if !ok || !os.SameFile(info, other) || info.Size() != other.Size() || info.ModTime() != other.ModTime() || !sameChangeTime(info, other) {
			return false
		}
	}
	return true
}

// ctime also changes when an in-place edit restores the original mtime. Keep
// platform-specific Stat_t layouts out of portable storage code; platforms
// without a known change-time field conservatively take the full proof path.
func sameChangeTime(a, b os.FileInfo) bool {
	x, xok := fileChangeTime(a)
	y, yok := fileChangeTime(b)
	// Unsupported platforms never obtain a valid directory cache stamp, but
	// may still complete a full proof using the original metadata checks.
	return !xok || !yok || reflect.DeepEqual(x, y)
}

// FileMetadataUnchanged is a conservative identity check for read-only caches.
// Platforms without a change-time field do not authorize cache reuse.
func FileMetadataUnchanged(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if _, ok := fileChangeTime(a); !ok {
		return false
	}
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime() && sameChangeTime(a, b)
}

func fileChangeTime(info os.FileInfo) (any, bool) {
	x := reflect.ValueOf(info.Sys())
	if x.Kind() != reflect.Pointer || x.IsNil() {
		return nil, false
	}
	x = x.Elem()
	if x.Kind() != reflect.Struct {
		return nil, false
	}
	for _, name := range []string{"Ctimespec", "Ctim"} {
		xf := x.FieldByName(name)
		if xf.IsValid() && xf.CanInterface() {
			return xf.Interface(), true
		}
	}
	return nil, false
}

// fileChangeInstant reads the same change-time field as fileChangeTime as a
// point in time, for comparing against the clock rather than another stamp.
func fileChangeInstant(info os.FileInfo) (time.Time, bool) {
	x := reflect.ValueOf(info.Sys())
	if x.Kind() != reflect.Pointer || x.IsNil() {
		return time.Time{}, false
	}
	x = x.Elem()
	if x.Kind() != reflect.Struct {
		return time.Time{}, false
	}
	for _, name := range []string{"Ctimespec", "Ctim"} {
		xf := x.FieldByName(name)
		if !xf.IsValid() || xf.Kind() != reflect.Struct {
			continue
		}
		sec, nsec := xf.FieldByName("Sec"), xf.FieldByName("Nsec")
		if !sec.CanInt() || !nsec.CanInt() {
			continue
		}
		return time.Unix(sec.Int(), nsec.Int()), true
	}
	return time.Time{}, false
}

func (g *ManagedSessionDeletionGuard) Close() error {
	if g == nil {
		return nil
	}
	var result error
	for index := len(g.files) - 1; index >= 0; index-- {
		file := g.files[index]
		result = errors.Join(result, unlockLease(file), file.Close())
	}
	g.files = nil
	g.sessions = managedSessionsStamp{}
	g.proofFiles = nil
	g.closed = true
	return result
}

func acquireExclusiveFileLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockLease(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !locked {
		_ = file.Close()
		return nil, fmt.Errorf("%w: writer is active", ErrManagedSessionBusy)
	}
	return file, nil
}

func managedSessionReferences(s *scanner) []ManagedSessionReference {
	references := make([]ManagedSessionReference, 0, len(s.managedStates))
	for sessionID, state := range s.managedStates {
		references = append(references, ManagedSessionReference{
			StoreDir: s.store, SessionID: sessionID, ManifestPath: cleanAbsolutePath(state.ManifestPath), ManifestSHA256: state.ManifestSHA256,
			BaseBytes: state.BaseBytes, BaseSHA256: state.BaseSHA256,
		})
	}
	sort.Slice(references, func(i, j int) bool { return references[i].SessionID < references[j].SessionID })
	return references
}
