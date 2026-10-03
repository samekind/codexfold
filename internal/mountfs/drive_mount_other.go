//go:build !windows

package mountfs

func prepareWindowsDriveMount(string) (bool, error)  { return false, nil }
func platformMountTarget(path string) string         { return path }
func platformMountSecurity(string) ([]string, error) { return nil, nil }
