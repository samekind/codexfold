//go:build windows

package sessionns

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func isNamespaceLink(info os.FileInfo) bool {
	// Go 1.23+ reports junctions as irregular name-surrogate reparse points.
	// Readlink and exact target validation still reject unrelated reparse data.
	return info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
}

func createNamespaceLink(target, link string) (result error) {
	if !filepath.IsAbs(target) || filepath.VolumeName(target) == "" {
		return errors.New("absolute local Windows junction target is required")
	}
	substitute, err := windows.UTF16FromString(`\??\` + filepath.Clean(target))
	if err != nil {
		return err
	}
	printed, err := windows.UTF16FromString(filepath.Clean(target))
	if err != nil {
		return err
	}
	data := make([]byte, 16+2*(len(substitute)+len(printed)))
	if len(data) > 16384 {
		return errors.New("Windows junction target exceeds reparse buffer limit")
	}
	binary.LittleEndian.PutUint32(data[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(data[4:6], uint16(len(data)-8))
	binary.LittleEndian.PutUint16(data[10:12], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(data[12:14], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(data[14:16], uint16(2*(len(printed)-1)))
	for i, value := range append(substitute, printed...) {
		binary.LittleEndian.PutUint16(data[16+2*i:18+2*i], value)
	}
	// Only create a fresh empty directory; never replace a pre-existing path.
	if err := os.Mkdir(link, 0o700); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			_ = os.Remove(link)
		}
	}()
	name, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var returned uint32
	return windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT,
		&data[0], uint32(len(data)), nil, 0, &returned, nil)
}
