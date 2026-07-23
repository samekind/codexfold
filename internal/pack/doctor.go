package pack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/samekind/codexfold/internal/fold"
)

type DoctorIssue struct {
	ObjectSHA256 string `json:"object_sha256,omitempty"`
	Message      string `json:"message"`
}

type DoctorResult struct {
	Generation    string        `json:"generation,omitempty"`
	ObjectCount   int           `json:"object_count"`
	VerifiedCount int           `json:"verified_count"`
	IssueCount    int           `json:"issue_count"`
	Issues        []DoctorIssue `json:"issues,omitempty"`
}

func Doctor(ctx context.Context, storeDir string) (DoctorResult, error) {
	resolver, err := Open(storeDir, OpenOptions{CacheBytes: -1})
	if err != nil {
		return DoctorResult{IssueCount: 1, Issues: []DoctorIssue{{Message: err.Error()}}}, nil
	}
	defer resolver.Close()
	result := DoctorResult{Generation: resolver.index.Generation, ObjectCount: len(resolver.index.Objects)}
	for _, object := range resolver.index.Objects {
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
	result.IssueCount = len(result.Issues)
	return result, nil
}

func verifyGeneration(ctx context.Context, directory string, index Index) error {
	resolver, err := openGeneration(directory, 0, false)
	if err != nil {
		return err
	}
	defer resolver.Close()
	for _, object := range index.Objects {
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
