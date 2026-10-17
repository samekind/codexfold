package mountfs

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
)

// This is a WinFsp local namespace, not an SMB share or a remote connection.
func windowsVolumePrefix(namespaceRoot, mountPoint string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(namespaceRoot) + "\x00" + filepath.Clean(mountPoint))))
	return fmt.Sprintf("/codexfold/%x", digest[:12])
}

// WindowsUNCPath is the stable local WinFsp alias used by Windows realpath.
func WindowsUNCPath(namespaceRoot, mountPoint string) string {
	return `\` + strings.ReplaceAll(windowsVolumePrefix(namespaceRoot, mountPoint), "/", `\`)
}
