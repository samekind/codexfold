package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jstar0/codexfold/internal/cli"
	"github.com/jstar0/codexfold/internal/launcher"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, stopLauncher, err := launcher.MonitorContext(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer stopLauncher()
	command := cli.NewRootCommand()
	command.SetContext(ctx)
	if err := command.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
