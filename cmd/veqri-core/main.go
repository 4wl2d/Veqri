package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/veqri/veqri/internal/buildinfo"
	"github.com/veqri/veqri/internal/coreapp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "veqri-core:", err)
		os.Exit(1)
	}
}

func run() error {
	info, err := buildinfo.Current()
	if err != nil {
		return fmt.Errorf("invalid build information: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return coreapp.Run(ctx, info)
}
