//go:build !windows

// Package dirsync isolates the platform-specific directory flush policy.
package dirsync

import "os"

func Sync(directory *os.File) error { return directory.Sync() }
