package cli

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/storage"
)

func TestStorageMaintenanceYieldDoesNotReportFailure(t *testing.T) {
	var diagnostics bytes.Buffer
	done := startStorageMaintenance(context.Background(), &diagnostics, t.TempDir(), func(context.Context, string) (storage.StorageGCResult, bool, error) {
		return storage.StorageGCResult{}, true, fmt.Errorf("foreground access: %w", storage.ErrManagedSessionBusy)
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("yielded maintenance did not finish")
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("ordinary foreground priority was reported as a maintenance failure: %s", diagnostics.String())
	}
}
