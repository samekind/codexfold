//go:build !darwin && !linux && !windows

package storage

import (
	"errors"
	"os"
)

func tryLockLease(*os.File) (bool, error) {
	return false, errors.New("storage leases are unsupported on this platform")
}

func unlockLease(*os.File) error {
	return errors.New("storage leases are unsupported on this platform")
}
