//go:build darwin

package service

import "testing"

func TestValidDarwinMountProviderAcceptsNativeFSKitAndFallbackOnly(t *testing.T) {
	tests := []struct {
		filesystem  string
		mountedFrom string
		want        bool
	}{
		{filesystem: "codexfold", mountedFrom: "file:///private/tmp/resource.bin", want: true},
		{filesystem: "CODEXFOLD", mountedFrom: "FILE:///private/tmp/resource.bin", want: true},
		{filesystem: "nfs", mountedFrom: "fuse-t:/private/tmp/resource", want: true},
		{filesystem: "nfs", mountedFrom: "server:/export", want: false},
		{filesystem: "apfs", mountedFrom: "/dev/disk1s1", want: false},
	}
	for _, test := range tests {
		if got := validDarwinMountProvider(test.filesystem, test.mountedFrom); got != test.want {
			t.Errorf("provider filesystem=%q mountedFrom=%q = %t, want %t", test.filesystem, test.mountedFrom, got, test.want)
		}
	}
}
