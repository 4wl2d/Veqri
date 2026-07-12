//go:build windows

package api

// The Windows service UI reports zero when the platform disk-space probe is
// unavailable; backup counts and integrity checks remain authoritative.
func availableDiskBytes(string) (int64, error) { return 0, nil }
