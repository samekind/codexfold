package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/jstar0/codexfold/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	command := cli.NewRootCommand()
	command.SetContext(ctx)
	if err := command.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
