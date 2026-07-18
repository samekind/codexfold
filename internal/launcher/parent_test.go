package launcher

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorParentCancelsWhenLauncherDisappears(t *testing.T) {
	var parent atomic.Int64
	parent.Store(42)
	ctx, cancel, err := monitorParent(context.Background(), "42", func() int { return int(parent.Load()) }, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	parent.Store(1)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("launcher parent loss did not cancel the context")
	}
}

func TestMonitorParentRejectsInvalidOrAlreadyLostLauncher(t *testing.T) {
	for _, value := range []string{"abc", "0", "1", "43"} {
		if _, _, err := monitorParent(context.Background(), value, func() int { return 42 }, time.Millisecond); err == nil {
			t.Fatalf("monitorParent(%q) succeeded", value)
		}
	}
}

func TestMonitorParentIsDisabledWithoutLauncherEnvironment(t *testing.T) {
	parent := context.Background()
	ctx, cancel, err := monitorParent(parent, "", func() int { return 1 }, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("unset launcher environment canceled an ordinary process")
	default:
	}
}
