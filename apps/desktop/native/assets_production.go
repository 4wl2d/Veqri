//go:build production

package main

import "embed"

//go:embed all:dist
var assets embed.FS
