//go:build !darwin

package service

import "errors"

func ProcessParentPID(int) (int, error) {
	return 0, errors.New("process parent inspection is available only on macOS")
}
