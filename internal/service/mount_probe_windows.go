//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/mountid"
	"golang.org/x/sys/windows"
)

func defaultMountProbe(path string) error {
	present, err := MountPresent(path)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("path is not a filesystem mount root")
	}
	name, err := windows.UTF16PtrFromString(filepath.Clean(path) + `\`)
	if err != nil {
		return err
	}
	var filesystem [256]uint16
	if err := windows.GetVolumeInformation(name, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil {
		return err
	}
	if !strings.EqualFold(windows.UTF16ToString(filesystem[:]), "FUSE") {
		return errors.New("path is not a WinFsp FUSE mount")
	}
	value, err := os.ReadFile(filepath.Join(path, mountid.Path))
	if err != nil {
		return err
	}
	return mountid.Validate(value)
}

func MountPresent(path string) (bool, error) {
	name, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(handle)
	var resolved [32768]uint16
	const volumeNameNT = 2
	n, err := windows.GetFinalPathNameByHandle(handle, &resolved[0], uint32(len(resolved)), volumeNameNT)
	if err != nil {
		return false, err
	}
	if n >= uint32(len(resolved)) {
		return false, fmt.Errorf("resolved mount path exceeds Windows path limit")
	}
	// NT volume roots resolve to \Device\<volume>\; a directory below that
	// volume has additional components, even when reached through a junction.
	return windowsMountRoot(windows.UTF16ToString(resolved[:n])), nil
}

func windowsMountRoot(resolved string) bool {
	parts := strings.Split(strings.Trim(resolved, `\`), `\`)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Device") {
		return true
	}
	// WinFsp network volumes resolve through MUP to a UNC share root. The
	// filesystem type and mount identity are checked separately.
	return len(parts) == 4 && strings.EqualFold(parts[0], "Device") &&
		strings.EqualFold(parts[1], "Mup") && strings.EqualFold(parts[2], "codexfold") && parts[3] != ""
}
