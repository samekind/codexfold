//go:build !windows

package fold

import "os"

func replaceFile(source string, target string) error {
	return os.Rename(source, target)
}
