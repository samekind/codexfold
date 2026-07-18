//go:build (darwin && fuse && cgo) || (windows && winfsp)

package mountfs

type noOpMountBacking struct{}

func prepareMountedBacking(string) (*noOpMountBacking, error) {
	return &noOpMountBacking{}, nil
}

func (*noOpMountBacking) Seal() error  { return nil }
func (*noOpMountBacking) Close() error { return nil }
