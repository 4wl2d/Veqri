package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/veqri/veqri/internal/config"
)

func TestDesktopRetentionSettingsAreAuthoritativeAndReadOnly(t *testing.T) {
	ctx := context.Background()
	store := openAuditAPITestStore(t)
	stale := defaultDesktopSettings(90)
	if err := store.SetSetting(ctx, "desktop", stale); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: config.Config{RetentionDays: 7}}

	updated, err := server.updateDesktopSettings(ctx, json.RawMessage(`{"theme":"light"}`))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Theme != "light" || updated.TranscriptRetentionDays != 7 || updated.AuditRetentionDays != 7 {
		t.Fatalf("desktop settings did not reflect authoritative Core retention: %+v", updated)
	}
	if _, err := server.updateDesktopSettings(ctx, json.RawMessage(`{"transcript_retention_days":1}`)); err == nil {
		t.Fatal("desktop API accepted a retention value that Core would not enforce")
	}
}
