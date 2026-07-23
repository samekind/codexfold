//go:build linux

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jstar0/codexfold/internal/mountid"
)

func defaultMountProbe(path string) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	want := filepath.Clean(path)
	fuseMount := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || filepath.Clean(unescapeLinuxMountField(fields[4])) != want {
			continue
		}
		separator := -1
		for index := 6; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index
				break
			}
		}
		if separator >= 0 && separator+2 < len(fields) && strings.HasPrefix(fields[separator+1], "fuse") && strings.Contains(strings.ToLower(unescapeLinuxMountField(fields[separator+2])), "codexfold") {
			fuseMount = true
		}
		break
	}
	if !fuseMount {
		return errors.New("path is not a CodexFold FUSE mount root")
	}
	return validateMountIdentity(path)
}

// MountPresent reports whether path is currently a mount root, independent of
// whether it is backed by CodexFold.
func MountPresent(path string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	want := filepath.Clean(path)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && filepath.Clean(unescapeLinuxMountField(fields[4])) == want {
			return true, nil
		}
	}
	return false, nil
}

func unescapeLinuxMountField(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}

func validateMountIdentity(path string) error {
	value, err := os.ReadFile(filepath.Join(path, mountid.Path))
	if err != nil {
		return err
	}
	return mountid.Validate(value)
}
