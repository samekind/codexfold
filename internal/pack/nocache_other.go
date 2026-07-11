//go:build !darwin

package pack

import "os"

func configureNoCache(*os.File) (bool, error) { return false, nil }
