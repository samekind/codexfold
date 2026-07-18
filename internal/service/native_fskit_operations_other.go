//go:build !darwin

package service

import (
	"errors"
)

func defaultNativeFSKitOperations() (NativeFSKitOperations, error) {
	return nil, errors.New("native FSKit supervision is available only on macOS")
}
