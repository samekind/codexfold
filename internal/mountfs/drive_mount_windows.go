//go:build windows

package mountfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func windowsDriveRoot(path string) (byte, bool) {
	path = filepath.Clean(path)
	if len(path) != 3 || path[1] != ':' || path[2] != '\\' {
		return 0, false
	}
	letter := strings.ToUpper(path[:1])[0]
	return letter, letter >= 'A' && letter <= 'Z'
}

func prepareWindowsDriveMount(path string) (bool, error) {
	letter, drive := windowsDriveRoot(path)
	if !drive {
		return false, nil
	}
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return true, err
	}
	if drives&(1<<uint(letter-'A')) != 0 {
		return true, fmt.Errorf("Windows mount drive %c: is already in use", letter)
	}
	return true, nil
}

func platformMountTarget(path string) string {
	letter, drive := windowsDriveRoot(path)
	if !drive {
		return path
	}
	// SCM mounts must be visible outside the LocalSystem logon session.
	if service, err := svc.IsWindowsService(); (err == nil && service) || os.Getenv("CODEXFOLD_WINDOWS_GLOBAL_MOUNT") == "1" {
		return fmt.Sprintf(`\\.\%c:`, letter)
	}
	return fmt.Sprintf("%c:", letter)
}

func platformMountSecurity(namespaceRoot string) ([]string, error) {
	if namespaceRoot == "" {
		return nil, errors.New("Windows mounts require a Codex home owner")
	}
	descriptor, err := windows.GetNamedSecurityInfo(namespaceRoot, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("read Codex home owner: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return nil, err
	}
	// A global volume must retain the home owner's privacy rather than
	// assigning each requesting user's identity as the file owner.
	sid := owner.String()
	return []string{"-o", "FileSecurity=O:" + sid + "D:P(A;;FA;;;SY)(A;;FA;;;" + sid + ")"}, nil
}
