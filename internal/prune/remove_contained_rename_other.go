//go:build !darwin && !linux && !windows

package prune

import "errors"

func renameRemovalNoReplace(string, string) error {
	return errors.New("atomic no-replace contained-session purge staging is unsupported on this platform")
}
