package api

import (
	"testing"
	"time"
)

func TestLocalOriginIncludesOnlyLoopbackAndPackagedWailsOrigins(t *testing.T) {
	allowed := []string{
		"http://localhost:5173", "https://127.0.0.1:8443", "wails://wails",
		"http://wails.localhost",
	}
	for _, origin := range allowed {
		if !localOrigin(origin) {
			t.Errorf("localOrigin(%q) = false, want true", origin)
		}
	}
	denied := []string{
		"https://example.com", "http://wails.localhost.evil.example", "wails://evil",
		"http://localhost.evil.example", "http://localhost:5173/path",
	}
	for _, origin := range denied {
		if localOrigin(origin) {
			t.Errorf("localOrigin(%q) = true, want false", origin)
		}
	}
}

func TestRetentionCutoffUsesUTCCalendarDaysAndZeroDisablesSweeps(t *testing.T) {
	now := time.Date(2026, time.March, 31, 23, 45, 0, 0, time.FixedZone("test", 2*60*60))
	if cutoff, enabled := retentionCutoff(now, 0); enabled || !cutoff.IsZero() {
		t.Fatalf("retentionCutoff(days=0) = (%v, %v), want disabled", cutoff, enabled)
	}
	cutoff, enabled := retentionCutoff(now, 30)
	if !enabled {
		t.Fatal("positive retention unexpectedly disabled")
	}
	want := now.UTC().AddDate(0, 0, -30)
	if !cutoff.Equal(want) || cutoff.Location() != time.UTC {
		t.Fatalf("retentionCutoff() = %v, want %v UTC", cutoff, want)
	}
}
