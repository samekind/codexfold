//go:build darwin

package mountfs

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in real-mount probe. Run this against an isolated native FSKit resource
// while updating only its daemon; the production mount is never a test target.
func TestNativeFSKitLiveDaemonUpdateContinuity(t *testing.T) {
	mount := os.Getenv("CODEXFOLD_LIVE_TEST_MOUNT")
	native := os.Getenv("CODEXFOLD_LIVE_TEST_NATIVE_ROOT")
	managed := os.Getenv("CODEXFOLD_LIVE_TEST_MANAGED")
	ready := os.Getenv("CODEXFOLD_LIVE_TEST_READY")
	if mount == "" || native == "" || managed == "" || ready == "" {
		t.Skip("isolated live-update mount and ready marker are required")
	}
	if !filepath.IsAbs(mount) || !filepath.IsAbs(native) || !filepath.IsAbs(managed) || !filepath.IsAbs(ready) {
		t.Fatal("live-update probe requires absolute isolated paths")
	}
	before, err := hashLiveProbeFile(managed)
	if err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join("sessions", "2099", "01", "01", "live-update-probe.jsonl")
	nativePath := filepath.Join(native, relative)
	mountedPath := filepath.Join(mount, relative)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativePath, []byte("{\"probe\":0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(nativePath)
		_ = os.Remove(ready)
	})
	visibilityDeadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(mountedPath); err == nil {
			break
		} else if time.Now().After(visibilityDeadline) {
			t.Fatalf("isolated native probe was not visible: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reads, writes, transients int
	var longestGap time.Duration
	lastSuccess := time.Now()
	deadline := time.Now().Add(30 * time.Second)
	for tick := 0; time.Now().Before(deadline); tick++ {
		file, err := os.Open(managed)
		if err == nil {
			var prefix [32]byte
			_, err = io.ReadFull(file, prefix[:])
			_ = file.Close()
			if err == nil {
				reads++
				gap := time.Since(lastSuccess)
				if gap > longestGap {
					longestGap = gap
				}
				lastSuccess = time.Now()
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				t.Fatalf("managed session path vanished during daemon switch: %v", err)
			}
			transients++
		}
		if tick%4 == 0 {
			f, openErr := os.OpenFile(mountedPath, os.O_APPEND|os.O_WRONLY, 0)
			if openErr == nil {
				_, openErr = fmt.Fprintf(f, "{\"probe\":%d}\n", tick+1)
				if openErr == nil {
					openErr = f.Sync()
				}
				openErr = errors.Join(openErr, f.Close())
			}
			if openErr != nil {
				if errors.Is(openErr, os.ErrNotExist) {
					t.Fatalf("native passthrough path vanished during daemon switch: %v", openErr)
				}
				transients++
			} else {
				writes++
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	after, err := hashLiveProbeFile(managed)
	if err != nil || after != before {
		t.Fatalf("managed session bytes changed: before=%x after=%x err=%v", before, after, err)
	}
	nativeBytes, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	mountedBytes, err := os.ReadFile(mountedPath)
	if err != nil || !bytes.Equal(nativeBytes, mountedBytes) {
		t.Fatalf("native passthrough bytes diverged: native=%d mounted=%d err=%v", len(nativeBytes), len(mountedBytes), err)
	}
	t.Logf("live update continuity: reads=%d writes=%d transient_errors=%d longest_success_gap=%s", reads, writes, transients, longestGap)
	if reads < 20 || writes < 5 || transients != 0 || longestGap >= 10*time.Second {
		t.Fatal("daemon handoff had a read/write failure or did not keep the isolated filesystem usable")
	}
}

func hashLiveProbeFile(path string) ([32]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [32]byte{}, err
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}
