//go:build windows

package enroll

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Policy/status writers publish by atomic replacement. Readers must allow
// delete sharing so a concurrent hot-policy update can replace their old file.
func readControlFile(path string) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	return io.ReadAll(file)
}

func replaceControlFile(source, target string) error {
	deadline := time.Now().Add(time.Second)
	for {
		err := os.Rename(source, target)
		if err == nil {
			return nil
		}
		if !(errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)) || time.Now().After(deadline) {
			return err
		}
		// An older host or external reader may briefly omit delete sharing.
		// Retry the atomic replacement; never remove the visible policy.
		time.Sleep(10 * time.Millisecond)
	}
}
