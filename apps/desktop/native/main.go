package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/veqri/veqri/internal/buildinfo"
	"github.com/veqri/veqri/internal/coreapp"
	"github.com/veqri/veqri/internal/managedcore"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

const (
	desktopBundleIdentifier = "ai.veqri.desktop"
	managedCoreArgument     = "--veqri-managed-core"
)

// appIcon is the single checked-in source for Wails runtime and platform
// packaging icons. Wails also converts this PNG to ICNS and ICO resources.
//
//go:embed build/appicon.png
var appIcon []byte

func main() {
	buildInfo, err := buildinfo.Current()
	if err != nil {
		fmt.Fprintln(os.Stderr, "veqri-desktop:", err)
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == managedCoreArgument {
		if err := runManagedCoreWithBuildInfo(buildInfo); err != nil {
			fmt.Fprintln(os.Stderr, "veqri-core:", err)
			os.Exit(1)
		}
		return
	}

	bridge := &Bridge{}
	if err := wails.Run(newApplicationOptions(bridge)); err != nil {
		log.Fatal(err)
	}
}

func newApplicationOptions(bridge *Bridge) *options.App {
	return &options.App{
		Title:       "Veqri",
		Width:       1440,
		Height:      940,
		MinWidth:    960,
		MinHeight:   680,
		AssetServer: &assetserver.Options{Assets: assets},
		Bind:        []any{bridge},
		OnStartup:   bridge.Startup,
		OnShutdown:  bridge.Shutdown,
		Linux: &linux.Options{
			Icon:             appIcon,
			ProgramName:      desktopBundleIdentifier,
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever,
		},
		Mac: &mac.Options{
			About: &mac.AboutInfo{
				Title: "Veqri",
				Icon:  appIcon,
			},
		},
	}
}

// runManagedCore turns the desktop executable into its own Core sidecar. The
// GUI starts this hidden mode with a pipe on stdin; closing the pipe cancels
// Core even on Windows, where POSIX process signals are not portable.
func runManagedCore() error {
	buildInfo, err := buildinfo.Current()
	if err != nil {
		return fmt.Errorf("load build identity: %w", err)
	}
	return runManagedCoreWithBuildInfo(buildInfo)
}

func runManagedCoreWithBuildInfo(buildInfo buildinfo.Info) error {
	if os.Getenv(managedcore.OwnerTokenEnvironment) == "" {
		return fmt.Errorf("%s is required; managed Core may only be started by the desktop supervisor", managedcore.OwnerTokenEnvironment)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	return coreapp.Run(ctx, buildInfo)
}
