//go:build linux

package mountfs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type linuxMountRecord struct {
	Filesystem string
	Source     string
}

func recoverStaleMount(mountPoint string) error {
	record, mounted := findLinuxMount(mountPoint)
	if !mounted {
		return nil
	}
	if !strings.HasPrefix(record.Filesystem, "fuse") || !strings.Contains(strings.ToLower(record.Source), "codexfold") {
		return fmt.Errorf("mount point is already used by %s source %s", record.Filesystem, record.Source)
	}
	_, healthErr := os.ReadFile(filepath.Join(mountPoint, ".codexfold-health"))
	if healthErr == nil {
		return errors.New("a healthy CodexFold mount is already active")
	}
	if !errors.Is(healthErr, syscall.ENOTCONN) && !errors.Is(healthErr, syscall.EIO) {
		return fmt.Errorf("CodexFold mount is not proven stale: %w", healthErr)
	}
	fusermount, err := exec.LookPath("fusermount3")
	if err != nil {
		return fmt.Errorf("locate fusermount3 for stale mount recovery: %w", err)
	}
	if output, err := exec.Command(fusermount, "-uz", mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("unmount stale CodexFold FUSE3 mount: %w: %s", err, strings.TrimSpace(string(output)))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, exists := findLinuxMount(mountPoint); !exists {
			return os.Chmod(mountPoint, 0o500)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("stale CodexFold FUSE3 mount remained after fusermount3")
}

func linuxFuseMountVisible(mountPoint string) bool {
	record, mounted := findLinuxMount(mountPoint)
	return mounted && strings.HasPrefix(record.Filesystem, "fuse")
}

func findLinuxMount(mountPoint string) (linuxMountRecord, bool) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return linuxMountRecord{}, false
	}
	want := filepath.Clean(mountPoint)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		separator := -1
		for index := 6; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+2 >= len(fields) {
			continue
		}
		mountedAt := unescapeLinuxMountField(fields[4])
		if filepath.Clean(mountedAt) == want {
			return linuxMountRecord{Filesystem: fields[separator+1], Source: unescapeLinuxMountField(fields[separator+2])}, true
		}
	}
	return linuxMountRecord{}, false
}

func unescapeLinuxMountField(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}
