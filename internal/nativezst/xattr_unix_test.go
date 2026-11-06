//go:build darwin || linux

package nativezst

import "golang.org/x/sys/unix"

func setTestXattr(path string, name string, value []byte) error {
	return unix.Setxattr(path, name, value, 0)
}

func getTestXattr(path string, name string, value []byte) (int, error) {
	return unix.Getxattr(path, name, value)
}

func removeTestXattr(path string, name string) error {
	return unix.Removexattr(path, name)
}
