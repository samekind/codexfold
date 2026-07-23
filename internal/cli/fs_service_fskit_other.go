//go:build !darwin

package cli

import (
	"context"
	"errors"
)

func prepareFSKitAppPlatform(context.Context, string, string) (fsKitAppTransaction, error) {
	return nil, errors.New("native FSKit app installation is available only on macOS")
}
