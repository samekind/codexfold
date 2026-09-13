//go:build !darwin

package cli

import (
	"context"
	"errors"
)

func prepareFSKitAppPlatform(context.Context, string, string, bool) (fsKitAppTransaction, error) {
	return nil, errors.New("native FSKit app installation is available only on macOS")
}

func ensureFSKitMenuBarResidency(context.Context, string) FSKitResidencyServiceOutcome {
	return FSKitResidencyServiceOutcome{State: "unavailable", Detail: "CodexFold menu-bar residency is available only on macOS"}
}

func reclaimNativeFSKitMount(context.Context, string, string) error {
	return nil
}

func reapIdleCodexFoldFSKitModuleProcesses(context.Context, string) error {
	return nil
}
