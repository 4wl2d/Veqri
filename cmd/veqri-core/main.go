package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/veqri/veqri/internal/coreapp"
)

var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "veqri-core:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return coreapp.Run(ctx, version)
}
