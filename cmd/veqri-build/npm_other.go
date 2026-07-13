//go:build !windows

package main

import "errors"

func runWindowsNPMWrapper(string, string, ...string) error {
	return errors.New("Windows npm wrapper is unavailable on this platform")
}
