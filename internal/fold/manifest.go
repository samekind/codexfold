package fold

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ManifestVersion = 1
	ManifestKind    = "fold-v1"
	PartResidual    = "residual"
	PartField       = "field"
)

type Manifest struct {
	Version   int              `json:"version"`
	Kind      string           `json:"kind"`
	CreatedAt string           `json:"created_at"`
	Session   ManifestSession  `json:"session"`
	Source    ManifestSource   `json:"source"`
	Settings  ManifestSettings `json:"settings"`
	Parts     []Part           `json:"parts"`
}

type ManifestSession struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	CWD         string `json:"cwd,omitempty"`
	RolloutPath string `json:"rollout_path"`
	Archived    bool   `json:"archived"`
}

type ManifestSource struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type ManifestSettings struct {
	FieldThreshold   int64  `json:"field_threshold"`
	MaxJSONLineBytes int64  `json:"max_json_line_bytes"`
	CDCMinBytes      int64  `json:"cdc_min_bytes"`
	CDCAverageBytes  int64  `json:"cdc_average_bytes"`
	CDCMaxBytes      int64  `json:"cdc_max_bytes"`
	Compression      string `json:"compression"`
}

type Part struct {
	Kind     string    `json:"kind"`
	JSONPath string    `json:"json_path,omitempty"`
	Object   ObjectRef `json:"object"`
}

func ManifestPath(storeDir string, sessionID string) string {
	return filepath.Join(storeDir, "manifests", sessionID+".json")
}

func LoadManifest(storeDir string, sessionID string) (Manifest, error) {
	if err := validateSessionID(sessionID); err != nil {
		return Manifest{}, err
	}
	return LoadManifestPath(ManifestPath(storeDir, sessionID))
}

func LoadManifestPath(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read fold manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode fold manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func writeManifest(storeDir string, manifest Manifest, overwrite bool) error {
	if err := validateSessionID(manifest.Session.ID); err != nil {
		return err
	}
	return writeManifestPath(ManifestPath(storeDir, manifest.Session.ID), manifest, overwrite)
}

func writeManifestPath(path string, manifest Manifest, overwrite bool) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("fold manifest already exists: %s", path)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fold manifest: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary manifest: %w", err)
	}
	commit := os.Rename
	if overwrite {
		commit = replaceFile
	}
	if err := commit(temporaryPath, path); err != nil {
		return fmt.Errorf("commit fold manifest: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

func validateManifest(manifest Manifest) error {
	if manifest.Version != ManifestVersion {
		return fmt.Errorf("unsupported fold manifest version %d", manifest.Version)
	}
	if manifest.Kind != ManifestKind {
		return fmt.Errorf("unsupported fold manifest kind %q", manifest.Kind)
	}
	if manifest.Session.ID == "" || manifest.Source.SHA256 == "" {
		return fmt.Errorf("fold manifest is missing session id or source SHA-256")
	}
	if err := validateSessionID(manifest.Session.ID); err != nil {
		return err
	}
	for index, part := range manifest.Parts {
		if part.Kind != PartResidual && part.Kind != PartField {
			return fmt.Errorf("fold manifest part %d has unsupported kind %q", index, part.Kind)
		}
		if len(part.Object.SHA256) != 64 || part.Object.RawBytes < 0 {
			return fmt.Errorf("fold manifest part %d has invalid object reference", index)
		}
	}
	return nil
}

func validateSessionID(sessionID string) error {
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, "/\\\x00") {
		return fmt.Errorf("unsafe fold session id %q", sessionID)
	}
	return nil
}

func newManifestTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
