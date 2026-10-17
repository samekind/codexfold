//go:build !windows

package enroll

import "os"

func readControlFile(path string) ([]byte, error) { return os.ReadFile(path) }

func replaceControlFile(source, target string) error { return os.Rename(source, target) }
