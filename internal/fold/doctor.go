package fold

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type DoctorIssue struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

type DoctorResult struct {
	StoreDir              string        `json:"store_dir"`
	ManifestCount         int           `json:"manifest_count"`
	VerifiedManifestCount int           `json:"verified_manifest_count"`
	ObjectReferenceCount  int           `json:"object_reference_count"`
	UniqueObjectCount     int           `json:"unique_object_count"`
	IssueCount            int           `json:"issue_count"`
	Issues                []DoctorIssue `json:"issues"`
}

func Doctor(ctx context.Context, storeDir string) (DoctorResult, error) {
	result := DoctorResult{StoreDir: storeDir, Issues: make([]DoctorIssue, 0)}
	manifests, loadIssues, err := loadAllManifests(storeDir)
	if err != nil {
		return DoctorResult{}, err
	}
	result.ManifestCount = len(manifests) + len(loadIssues)
	result.Issues = append(result.Issues, loadIssues...)
	store := NewObjectStore(storeDir)
	unique := make(map[string]ObjectRef)
	for _, manifest := range manifests {
		if err := ctx.Err(); err != nil {
			return DoctorResult{}, err
		}
		for _, part := range manifest.Parts {
			result.ObjectReferenceCount++
			unique[part.Object.SHA256] = part.Object
		}
		if err := verifyStoredManifest(ctx, store, manifest); err != nil {
			result.Issues = append(result.Issues, DoctorIssue{
				Scope: "manifest", Path: ManifestPath(storeDir, manifest.Session.ID), Error: err.Error(),
			})
		} else {
			result.VerifiedManifestCount++
		}
	}
	result.UniqueObjectCount = len(unique)
	for digest, ref := range unique {
		if err := ctx.Err(); err != nil {
			return DoctorResult{}, err
		}
		if _, err := store.Read(ref); err != nil {
			result.Issues = append(result.Issues, DoctorIssue{
				Scope: "object", Path: store.ObjectPath(digest), Error: err.Error(),
			})
		}
	}
	result.IssueCount = len(result.Issues)
	return result, nil
}

func loadAllManifests(storeDir string) ([]Manifest, []DoctorIssue, error) {
	manifestDir := filepath.Join(storeDir, "manifests")
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Manifest{}, []DoctorIssue{}, nil
		}
		return nil, nil, fmt.Errorf("read manifest directory: %w", err)
	}
	manifests := make([]Manifest, 0, len(entries))
	issues := make([]DoctorIssue, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		manifest, err := LoadManifest(storeDir, sessionID)
		if err != nil {
			issues = append(issues, DoctorIssue{Scope: "manifest", Path: filepath.Join(manifestDir, entry.Name()), Error: err.Error()})
			continue
		}
		manifests = append(manifests, manifest)
	}
	return manifests, issues, nil
}

func walkObjectFiles(storeDir string, visit func(path string, info fs.FileInfo) error) error {
	objectDir := filepath.Join(storeDir, "objects")
	err := filepath.Walk(objectDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && filepath.Ext(path) == ".zst" {
			return visit(path, info)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
