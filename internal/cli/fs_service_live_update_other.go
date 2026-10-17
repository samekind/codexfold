//go:build !windows && !darwin

package cli

import "github.com/spf13/cobra"

func addWindowsLiveDaemonUpdateCommand(*cobra.Command) {}
