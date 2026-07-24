package pack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/samekind/codexfold/internal/fold"
)

type DoctorIssue struct {
	ObjectSHA256 string `json:"object_sha256,omitempty"`
	Message      string `json:"message"`
}

type DoctorResult struct {
	Generation            string        `json:"generation,omitempty"`
	ObjectCount           int64         `json:"object_count"`
	VerifiedCount         int64         `json:"verified_count"`
	ManifestCount         int           `json:"manifest_count"`
	VerifiedManifestCount int           `json:"verified_manifest_count"`
	IssueCount            int           `json:"issue_count"`
	Issues                []DoctorIssue `json:"issues,omitempty"`
}

func Doctor(ctx context.Context, storeDir string) (DoctorResult, error) {
	resolver, err := Open(storeDir, OpenOptions{CacheBytes: -1})
	if err != nil {
		return DoctorResult{IssueCount: 1, Issues: []DoctorIssue{{Message: err.Error()}}}, nil
	}
	defer resolver.Close()
	result := DoctorResult{Generation: resolver.Generation(), ObjectCount: resolver.ObjectCount()}
	for position := int64(0); position < resolver.ObjectCount(); position++ {
		object, objectErr := resolver.objectAt(position)
		if objectErr != nil {
			result.Issues = append(result.Issues, DoctorIssue{Message: objectErr.Error()})
			continue
		}
		hasher := sha256.New()
		buffer := make([]byte, 128<<10)
		var offset int64
		failed := false
		for offset < object.RawBytes {
			n, readErr := resolver.ReadAt(ctx, fold.ObjectRef{SHA256: object.SHA256, RawBytes: object.RawBytes}, buffer, offset)
			if n > 0 {
				_, _ = hasher.Write(buffer[:n])
				offset += int64(n)
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				result.Issues = append(result.Issues, DoctorIssue{ObjectSHA256: object.SHA256, Message: readErr.Error()})
				failed = true
				break
			}
			if n == 0 {
				break
			}
		}
		if !failed && (offset != object.RawBytes || hex.EncodeToString(hasher.Sum(nil)) != object.SHA256) {
			result.Issues = append(result.Issues, DoctorIssue{ObjectSHA256: object.SHA256, Message: fmt.Sprintf("object reconstruction mismatch at %d of %d bytes", offset, object.RawBytes)})
			failed = true
		}
		if !failed {
			result.VerifiedCount++
		}
	}
	if err := verifyPackedManifests(ctx, storeDir, resolver, &result); err != nil {
		return DoctorResult{}, err
	}
	result.IssueCount = len(result.Issues)
	return result, nil
}

func verifyPackedManifests(ctx context.Context, storeDir string, resolver *Resolver, result *DoctorResult) error {
	manifestRoot := filepath.Join(storeDir, "manifests")
	err := filepath.WalkDir(manifestRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		result.ManifestCount++
		manifest, err := fold.LoadManifestPath(path)
		if err != nil {
			result.Issues = append(result.Issues, DoctorIssue{Message: fmt.Sprintf("manifest %s: %v", path, err)})
			return nil
		}
		hasher := sha256.New()
		buffer := make([]byte, 1<<20)
		var manifestBytes int64
		for partIndex, part := range manifest.Parts {
			var objectOffset int64
			for objectOffset < part.Object.RawBytes {
				need := len(buffer)
				if remaining := part.Object.RawBytes - objectOffset; int64(need) > remaining {
					need = int(remaining)
				}
				n, readErr := resolver.ReadAt(ctx, part.Object, buffer[:need], objectOffset)
				if n > 0 {
					_, _ = hasher.Write(buffer[:n])
					objectOffset += int64(n)
					manifestBytes += int64(n)
				}
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					result.Issues = append(result.Issues, DoctorIssue{Message: fmt.Sprintf("manifest %s part %d: %v", path, partIndex, readErr)})
					return nil
				}
				if n == 0 {
					break
				}
			}
			if objectOffset != part.Object.RawBytes {
				result.Issues = append(result.Issues, DoctorIssue{Message: fmt.Sprintf("manifest %s part %d: packed object ended at %d of %d bytes", path, partIndex, objectOffset, part.Object.RawBytes)})
				return nil
			}
		}
		if manifestBytes != manifest.Source.Bytes || hex.EncodeToString(hasher.Sum(nil)) != manifest.Source.SHA256 {
			result.Issues = append(result.Issues, DoctorIssue{Message: fmt.Sprintf("manifest %s: packed reconstruction mismatch", path)})
			return nil
		}
		result.VerifiedManifestCount++
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func verifyGeneration(ctx context.Context, directory string) error {
	resolver, err := openGeneration(directory, 0, false)
	if err != nil {
		return err
	}
	defer resolver.Close()
	for position := int64(0); position < resolver.ObjectCount(); position++ {
		object, err := resolver.objectAt(position)
		if err != nil {
			return err
		}
		hasher := sha256.New()
		buffer := make([]byte, 128<<10)
		var offset int64
		for offset < object.RawBytes {
			n, readErr := resolver.ReadAt(ctx, fold.ObjectRef{SHA256: object.SHA256, RawBytes: object.RawBytes}, buffer, offset)
			if n > 0 {
				_, _ = hasher.Write(buffer[:n])
				offset += int64(n)
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			if n == 0 {
				break
			}
		}
		if offset != object.RawBytes || hex.EncodeToString(hasher.Sum(nil)) != object.SHA256 {
			return fmt.Errorf("packed object %s verification failed", object.SHA256)
		}
	}
	return nil
}
