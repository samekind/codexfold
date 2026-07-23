package fold

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/samekind/codexfold/internal/storage"
)

type DoctorIssue struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

type DoctorResult struct {
	StoreDir              string            `json:"store_dir"`
	ManifestCount         int               `json:"manifest_count"`
	VerifiedManifestCount int               `json:"verified_manifest_count"`
	ObjectReferenceCount  int               `json:"object_reference_count"`
	UniqueObjectCount     int               `json:"unique_object_count"`
	IssueCount            int               `json:"issue_count"`
	Issues                []DoctorIssue     `json:"issues"`
	Storage               storage.Inventory `json:"storage"`
	StorageLimits         storage.Limits    `json:"storage_limits"`
	AvailableBytes        int64             `json:"available_bytes"`
}

type loadedManifest struct {
	Path     string
	Manifest Manifest
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
	for _, loaded := range manifests {
		manifest := loaded.Manifest
		if err := ctx.Err(); err != nil {
			return DoctorResult{}, err
		}
		for _, part := range manifest.Parts {
			result.ObjectReferenceCount++
			unique[part.Object.SHA256] = part.Object
		}
		if err := verifyStoredManifest(ctx, store, manifest); err != nil {
			result.Issues = append(result.Issues, DoctorIssue{
				Scope: "manifest", Path: loaded.Path, Error: err.Error(),
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
	result.Storage, err = storage.Scan(ctx, storage.Options{StoreDir: storeDir, AllowMetadataIssues: true})
	if err != nil {
		result.Issues = append(result.Issues, DoctorIssue{Scope: "storage", Path: storeDir, Error: err.Error()})
	} else {
		for _, issue := range result.Storage.Issues {
			result.Issues = append(result.Issues, DoctorIssue{Scope: "storage", Path: storeDir, Error: issue})
		}
	}
	result.StorageLimits, err = storage.LoadLimits(storeDir)
	if err != nil {
		result.Issues = append(result.Issues, DoctorIssue{Scope: "storage-policy", Path: filepath.Join(storeDir, storage.PolicyFilename), Error: err.Error()})
	}
	result.AvailableBytes, err = storage.AvailableBytes(storeDir)
	if err != nil {
		result.Issues = append(result.Issues, DoctorIssue{Scope: "storage-space", Path: storeDir, Error: err.Error()})
	}
	result.IssueCount = len(result.Issues)
	return result, nil
}

func loadAllManifests(storeDir string) ([]loadedManifest, []DoctorIssue, error) {
	manifestDir := filepath.Join(storeDir, "manifests")
	manifests := make([]loadedManifest, 0)
	issues := make([]DoctorIssue, 0)
	err := filepath.WalkDir(manifestDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		manifest, err := LoadManifestPath(path)
		if err != nil {
			issues = append(issues, DoctorIssue{Scope: "manifest", Path: path, Error: err.Error()})
			return nil
		}
		manifests = append(manifests, loadedManifest{Path: path, Manifest: manifest})
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return []loadedManifest{}, []DoctorIssue{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest directory: %w", err)
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
