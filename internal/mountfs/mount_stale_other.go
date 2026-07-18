//go:build !linux

package mountfs

func recoverStaleMount(string) error { return nil }
