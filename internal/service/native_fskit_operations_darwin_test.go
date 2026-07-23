//go:build darwin

package service

import (
	"reflect"
	"testing"
)

func TestNativeFSKitMountArgumentsForceFSKitModule(t *testing.T) {
	want := []string{"-F", "-t", "codexfoldnative", "/tmp/resource", "/tmp/mount"}
	if got := nativeFSKitMountArguments("/tmp/resource", "/tmp/mount"); !reflect.DeepEqual(got, want) {
		t.Fatalf("mount arguments = %v, want %v", got, want)
	}
}
