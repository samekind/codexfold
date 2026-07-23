//go:build darwin

package service

import (
	"os"
	"testing"
)

func TestProcessParentPIDReportsCurrentParent(t *testing.T) {
	parent, err := ProcessParentPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if parent != os.Getppid() {
		t.Fatalf("parent PID = %d, want %d", parent, os.Getppid())
	}
}
