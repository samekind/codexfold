//go:build windows

package enroll

import (
	"os"
	"testing"
	"time"
)

func TestPolicyUpdateWaitsForLegacyReaderWithoutDeleteSharing(t *testing.T) {
	path := ControlPath(t.TempDir())
	control := Control{Interval: time.Minute, StableFor: time.Hour, BatchSize: 1}
	if err := SaveControl(path, control); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); _ = reader.Close(); close(closed) }()
	control.Enabled = true
	err = SaveControl(path, control)
	<-closed
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadControl(path)
	if err != nil || !got.Enabled {
		t.Fatalf("updated policy not visible: %#v %v", got, err)
	}
}
