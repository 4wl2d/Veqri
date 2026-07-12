//go:build !production

package main

import "os"

// Unit tests compile the main package before the generated frontend exists.
// Production builds use assets_production.go after build-native.mjs creates dist.
var assets = os.DirFS("dist")
