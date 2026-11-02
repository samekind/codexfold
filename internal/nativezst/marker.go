// Package nativezst folds the rollouts Codex compresses itself.
//
// Codex compresses an idle rollout to a sibling rollout-*.jsonl.zst and
// deletes the .jsonl. Those files are immutable: Codex only reads, renames and
// deletes them. A folded .zst keeps its path, inode, size, mode and times, but
// its data blocks are released: a marker extended attribute names the fold
// manifest that holds the decoded content, and the mount re-encodes the
// original bytes on read with the exact libzstd profile Codex used.
//
// The marker is written and flushed before any block is released, and every
// state in between still serves correct bytes through the mount: either the
// file still holds its own data, or the marker points at content proven to
// re-encode to it.
package nativezst

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codexzstd"
)

const (
	// MarkerAttribute is hidden from clients of the mount.
	MarkerAttribute = "user.codexfold.native-zst"
	MarkerKind      = "codexfold-native-zst"
	MarkerVersion   = 1
	Suffix          = ".jsonl.zst"
	manifestDir     = "native-zst"
	stateDir        = "native-zst"
	maxMarkerBytes  = 4 << 10
)

var (
	ErrUnsupported = errors.New("folded Codex .zst files are unsupported on this platform")
	ErrChanged     = errors.New("native .zst changed before it could be folded")
	// ErrMarkerKept means a conversion failed after the file was bound to its
	// manifest; the manifest must not be discarded.
	ErrMarkerKept = errors.New("native .zst kept its marker")
)

// AllocatedBytes is the disk space a file's data blocks occupy.
func AllocatedBytes(info os.FileInfo) int64 { return allocatedBytes(info) }

// ChangedAfter reports whether the file's metadata changed after t.
func ChangedAfter(info os.FileInfo, t time.Time) bool { return changeTime(info).After(t) }

// Marker is stored on a folded file and binds it to its fold manifest.
type Marker struct {
	Version       int    `json:"version"`
	Kind          string `json:"kind"`
	Profile       string `json:"profile"`
	Manifest      string `json:"manifest"`
	EncodedBytes  int64  `json:"encoded_bytes"`
	EncodedSHA256 string `json:"encoded_sha256"`
	ContentBytes  int64  `json:"content_bytes"`
	ContentSHA256 string `json:"content_sha256"`
}

func (m Marker) Validate() error {
	if m.Version != MarkerVersion || m.Kind != MarkerKind {
		return fmt.Errorf("unsupported native .zst marker %s v%d", m.Kind, m.Version)
	}
	if m.Profile != codexzstd.Profile {
		return fmt.Errorf("native .zst was folded with encoder profile %q, this build has %q", m.Profile, codexzstd.Profile)
	}
	if _, err := ManifestKey(m.Manifest); err != nil {
		return err
	}
	if m.EncodedBytes <= 0 || m.ContentBytes < 0 || !validDigest(m.EncodedSHA256) || !validDigest(m.ContentSHA256) {
		return errors.New("native .zst marker has invalid sizes or digests")
	}
	return nil
}

func (m Marker) encode() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func decodeMarker(data []byte) (Marker, error) {
	var marker Marker
	if err := json.Unmarshal(data, &marker); err != nil {
		return Marker{}, fmt.Errorf("decode native .zst marker: %w", err)
	}
	return marker, marker.Validate()
}

// ManifestKey returns the key of a marker manifest path such as
// manifests/native-zst/<key>.json.
func ManifestKey(manifest string) (string, error) {
	if path.Dir(manifest) != "manifests/"+manifestDir || path.Ext(manifest) != ".json" {
		return "", fmt.Errorf("unsafe native .zst manifest %q", manifest)
	}
	key := strings.TrimSuffix(path.Base(manifest), ".json")
	if !strings.HasPrefix(key, "zst-") || strings.ContainsAny(key, `/\.`+"\x00") {
		return "", fmt.Errorf("unsafe native .zst manifest %q", manifest)
	}
	return key, nil
}

// ManifestRoot holds one manifest per folded file. Every manifest walker in
// the store (pack references, recovery, GC, doctor) descends into it, so its
// objects stay referenced exactly as long as the manifest exists.
func ManifestRoot(store string) string {
	return filepath.Join(store, "manifests", manifestDir)
}

func manifestRelative(key string) string { return "manifests/" + manifestDir + "/" + key + ".json" }

func ManifestPath(store string, marker Marker) (string, error) {
	key, err := ManifestKey(marker.Manifest)
	if err != nil {
		return "", err
	}
	return filepath.Join(ManifestRoot(store), key+".json"), nil
}

func StateRoot(store string) string { return filepath.Join(store, stateDir) }

// IsCandidateName reports whether a file name is a Codex-compressed rollout.
func IsCandidateName(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, Suffix) && !strings.HasPrefix(name, "._")
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}
