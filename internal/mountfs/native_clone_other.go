//go:build !darwin

package mountfs

import "errors"

func cloneNativeFile(string, string) error {
	return errors.New("file cloning is not supported on this platform")
}
