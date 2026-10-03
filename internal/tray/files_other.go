//go:build !windows

package tray

import "os"

func openSharedFile(path string) (*os.File, error)   { return os.Open(path) }
func replaceUIFile(source, destination string) error { return os.Rename(source, destination) }
