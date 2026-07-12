package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:dist
var assets embed.FS

func main() {
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
