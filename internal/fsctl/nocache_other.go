//go:build !darwin

package fsctl

import "os"

func configureNoCache(*os.File) (bool, error) { return false, nil }
