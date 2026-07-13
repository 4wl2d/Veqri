package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/veqri/veqri/internal/coreapp"
	"github.com/veqri/veqri/internal/managedcore"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

const managedCoreArgument = "--veqri-managed-core"

var version = "0.1.0-dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == managedCoreArgument {
		if err := runManagedCore(); err != nil {
			fmt.Fprintln(os.Stderr, "veqri-core:", err)
			os.Exit(1)
		}
		return
	}

	bridge := &Bridge{}
	if err := wails.Run(&options.App{
		Title:       "Veqri",
		Width:       1440,
		Height:      940,
		MinWidth:    960,
		MinHeight:   680,
		AssetServer: &assetserver.Options{Assets: assets},
		Bind:        []any{bridge},
		OnStartup:   bridge.Startup,
		OnShutdown:  bridge.Shutdown,
	}); err != nil {
		log.Fatal(err)
	}
}

// runManagedCore turns the desktop executable into its own Core sidecar. The
// GUI starts this hidden mode with a pipe on stdin; closing the pipe cancels
// Core even on Windows, where POSIX process signals are not portable.
func runManagedCore() error {
	if os.Getenv(managedcore.OwnerTokenEnvironment) == "" {
		return fmt.Errorf("%s is required; managed Core may only be started by the desktop supervisor", managedcore.OwnerTokenEnvironment)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	return coreapp.Run(ctx, version)
}
