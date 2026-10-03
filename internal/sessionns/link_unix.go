//go:build !windows

package sessionns

import "os"

func isNamespaceLink(info os.FileInfo) bool         { return info.Mode()&os.ModeSymlink != 0 }
func createNamespaceLink(target, link string) error { return os.Symlink(target, link) }
