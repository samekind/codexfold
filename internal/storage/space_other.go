//go:build !darwin && !linux && !windows

package storage

import "errors"

func AvailableBytes(string) (int64, error) {
	return 0, errors.New("available-byte probe is unsupported on this platform")
}
