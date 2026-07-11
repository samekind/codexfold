//go:build windows

package vfs

import (
	"fmt"
	"syscall"
	"unsafe"
)

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceStateFile(source string, target string) error {
	sourcePointer, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPointer, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileExW.Call(uintptr(unsafe.Pointer(sourcePointer)), uintptr(unsafe.Pointer(targetPointer)), 0x1|0x8)
	if result == 0 {
		return fmt.Errorf("replace file: %w", callErr)
	}
	return nil
}

func syncStateDirectory(string) error { return nil }
