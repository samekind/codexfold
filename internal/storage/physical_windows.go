//go:build windows

package storage

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func physicalFile(path string, info os.FileInfo) (string, int64, error) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", 0, err
	}
	handle, err := windows.CreateFile(utf16Path, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return "", 0, fmt.Errorf("open physical file identity for %s: %w", path, err)
	}
	defer windows.CloseHandle(handle)
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &identity); err != nil {
		return "", 0, fmt.Errorf("read physical file identity for %s: %w", path, err)
	}
	key := fmt.Sprintf("%d:%d:%d", identity.VolumeSerialNumber, identity.FileIndexHigh, identity.FileIndexLow)
	return key, info.Size(), nil
}

func physicalLinkCount(path string, _ os.FileInfo) (uint64, error) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(utf16Path, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &identity); err != nil {
		return 0, err
	}
	return uint64(identity.NumberOfLinks), nil
}
