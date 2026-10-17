//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"testing"
)

func TestWindowsIconResourcesAndDPIManifest(t *testing.T) {
	var instance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
		t.Fatal(err)
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	for _, resource := range []uintptr{2, 12} {
		for _, size := range []uintptr{16, 24, 32, 48, 64} {
			icon, _, err := user32.NewProc("LoadImageW").Call(uintptr(instance), resource, 1, size, size, 0)
			if icon == 0 {
				t.Fatalf("resource %d at %d pixels: %v", resource, size, err)
			}
			user32.NewProc("DestroyIcon").Call(icon)
		}
	}
	if contextProc := user32.NewProc("GetThreadDpiAwarenessContext"); contextProc.Find() == nil {
		context, _, _ := contextProc.Call()
		equal, _, _ := user32.NewProc("AreDpiAwarenessContextsEqual").Call(context, ^uintptr(3))
		if equal == 0 {
			t.Fatal("tray executable does not enable PerMonitorV2 through its manifest")
		}
	}
}
