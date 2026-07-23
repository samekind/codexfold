//go:build darwin

package service

import (
	"os"
	"testing"
)

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

func TestMountPresentRejectsOrdinaryDirectory(t *testing.T) {
	path := t.TempDir()
	present, err := MountPresent(path)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("ordinary directory %s reported as a mount root", path)
	}
	missing := path + ".missing"
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("test path unexpectedly exists: %v", err)
	}
	present, err = MountPresent(missing)
	if err != nil || present {
		t.Fatalf("missing path mount presence = %t, %v", present, err)
	}
}
