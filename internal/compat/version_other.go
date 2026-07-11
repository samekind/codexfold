//go:build !darwin

package compat

import (
	"context"
	"errors"
)

func DetectDesktopVersion(context.Context, string) (ClientVersion, error) {
	return ClientVersion{}, errors.New("desktop version detection is not implemented on this platform")
}
