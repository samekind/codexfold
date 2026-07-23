//go:build (linux && fuse && fuse3 && cgo) || (windows && winfsp)

package mountfs

func validateFuseProvider() error { return nil }
