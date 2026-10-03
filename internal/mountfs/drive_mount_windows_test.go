//go:build windows

package mountfs

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsDriveMountRejectsOccupiedDrive(t *testing.T) {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	for letter := byte('A'); letter <= 'Z'; letter++ {
		if drives&(1<<uint(letter-'A')) == 0 {
			continue
		}
		handled, err := prepareWindowsDriveMount(string(letter) + `:\`)
		if !handled || err == nil {
			t.Fatal("occupied drive accepted for replacement")
		}
		return
	}
	t.Fatal("no occupied drive found")
}

func TestWindowsDriveMountNaming(t *testing.T) {
	t.Setenv("CODEXFOLD_WINDOWS_GLOBAL_MOUNT", "")
	if actual := platformMountTarget(`t:\`); actual != "T:" {
		t.Fatalf("target=%q", actual)
	}
	first := WindowsUNCPath(`C:\Users\one\.codex`, `T:\`)
	second := WindowsUNCPath(`C:\Users\two\.codex`, `T:\`)
	if first == second || !strings.HasPrefix(first, `\\codexfold\`) {
		t.Fatal("namespace is not home-scoped")
	}
}

func TestWindowsServiceChildUsesGlobalDrive(t *testing.T) {
	t.Setenv("CODEXFOLD_WINDOWS_GLOBAL_MOUNT", "1")
	if actual := platformMountTarget(`u:\`); actual != `\\.\U:` {
		t.Fatalf("service child target=%q", actual)
	}
}

func TestWindowsMountSecurityKeepsHomeOwner(t *testing.T) {
	arguments, err := platformMountSecurity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(strings.TrimPrefix(arguments[1], "FileSecurity="))
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		t.Fatalf("mount has no home owner: %v", err)
	}
	if strings.Contains(arguments[1], ";;;WD") || strings.Contains(arguments[1], ";;;AU") {
		t.Fatal("private session mount grants access to other users")
	}
	if _, err := platformMountSecurity(""); err == nil {
		t.Fatal("mount with no known home owner was accepted")
	}
}
