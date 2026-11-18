//go:build postgres

package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestPostgresDailyStore exercises the daily_backup_settings table.
func TestPostgresDailyStore(t *testing.T) {
	t.Setenv(DatabaseURLEnv, postgresTestURL(t))
	t.Setenv(TokenKeyEnv, "deployment-key")
	s, err := New(discardLogger(), newDeployApp(t), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("New with PostgreSQL: %v", err)
	}
	userID := time.Now().UnixNano()
	ctx := context.Background()
	store, ok := s.daily.(*postgresDailyStore)
	if !ok {
		t.Fatalf("daily store = %T, want *postgresDailyStore", s.daily)
	}
	t.Cleanup(func() {
		_ = store.Delete(ctx, userID)
		_ = s.Close()
	})

	if got, err := store.Get(ctx, userID); err != nil || got.Enabled {
		t.Fatalf("Get missing row = %+v, %v", got, err)
	}
	if err := store.SetEnabled(ctx, userID, true); err != nil {
		t.Fatal(err)
	}
	ids, err := store.ListEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		found = found || id == userID
	}
	if !found {
		t.Fatalf("ListEnabled = %v, missing %d", ids, userID)
	}
	run := DailySettings{UserID: userID, NeedsReauth: reauthDrive, LastRunAt: time.Now().UTC().Truncate(time.Second),
		LastStatus: dailyStatusNeedsReauth, LastMessage: "m", LastOK: 1, LastFailed: 2, LastSkipped: 3}
	if err := store.RecordRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, userID)
	if err != nil || !got.Enabled || got.NeedsReauth != reauthDrive || got.LastOK != 1 || got.LastFailed != 2 ||
		got.LastSkipped != 3 || !got.LastRunAt.Equal(run.LastRunAt) {
		t.Fatalf("Get after RecordRun = %+v, %v", got, err)
	}
	if err := store.ClearNeedsReauth(ctx, userID, reauthDrive); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, userID); got.NeedsReauth != "" {
		t.Fatalf("needs_reauth not cleared: %+v", got)
	}
	if err := store.Delete(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, userID); got.Enabled {
		t.Fatal("row survived Delete")
	}
}
