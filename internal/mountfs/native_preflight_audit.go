package mountfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type NativePreflightIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type NativePreflightAudit struct {
	NativePreflightReport
	Issues []NativePreflightIssue `json:"issues,omitempty"`
}

func AuditNativeWriterRollouts(ctx context.Context, nativeRoot string) (NativePreflightAudit, error) {
	root := filepath.Clean(nativeRoot)
	activeRoot := filepath.Join(root, "sessions")
	report := NativePreflightAudit{}
	err := filepath.WalkDir(activeRoot, func(filePath string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			report.Issues = append(report.Issues, NativePreflightIssue{Path: filePath, Message: walkErr.Error()})
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			report.Issues = append(report.Issues, NativePreflightIssue{Path: filePath, Message: "symlink is not allowed in the active native rollout tree"})
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			report.Issues = append(report.Issues, NativePreflightIssue{Path: filePath, Message: "non-regular file is not allowed in the active native rollout tree"})
			return nil
		}
		if strings.HasPrefix(entry.Name(), "._") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			report.Issues = append(report.Issues, NativePreflightIssue{Path: filePath, Message: err.Error()})
			return nil
		}
		report.Files++
		report.Bytes += info.Size()
		validated, err := validateNativeJSONL(ctx, filePath)
		report.ValidatedBytes += validated
		report.ValidatedFiles++
		if err != nil {
			report.Issues = append(report.Issues, NativePreflightIssue{Path: filePath, Message: err.Error()})
		}
		return nil
	})
	if os.IsNotExist(err) {
		return NativePreflightAudit{}, nil
	}
	if err != nil {
		return report, fmt.Errorf("audit active native rollouts: %w", err)
	}
	return report, nil
}
