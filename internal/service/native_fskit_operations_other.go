//go:build !darwin

package service

import (
	"context"
	"errors"
)

func defaultNativeFSKitOperations() (NativeFSKitOperations, error) {
	return nil, errors.New("native FSKit supervision is available only on macOS")
}

func nativeFSKitOperationsForType(string) (NativeFSKitOperations, error) {
	return nil, errors.New("native FSKit supervision is available only on macOS")
}

// UnmountNativeFSKit is only meaningful on macOS; elsewhere there is no native
// FSKit mount to reclaim.
func UnmountNativeFSKit(ctx context.Context, mountPoint string, force bool) error {
	return errors.New("native FSKit supervision is available only on macOS")
}
