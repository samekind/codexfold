//go:build windows

package prune

import (
	"fmt"
	"syscall"
	"unsafe"
)

var moveStateFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceStateFile(source string, target string) error {
	sourcePointer, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPointer, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	result, _, callErr := moveStateFileExW.Call(uintptr(unsafe.Pointer(sourcePointer)), uintptr(unsafe.Pointer(targetPointer)), 0x1|0x8)
	if result == 0 {
		return fmt.Errorf("replace state file: %w", callErr)
	}
	return nil
}

func syncStateDirectory(string) error {
	return nil
}
