package persistence

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCreateBackupPublishesVerifiedPrivateSnapshot(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.SetSetting(ctx, "backup-fixture", map[string]string{"value": "durable snapshot"}); err != nil {
		t.Fatal(err)
	}

	backupDirectory := filepath.Join(t.TempDir(), "backups with space")
	path, err := store.CreateBackup(ctx, backupDirectory)
	if err != nil {
		t.Fatalf("CreateBackup(): %v", err)
	}
	if filepath.Dir(path) != backupDirectory || filepath.Ext(path) != ".db" || strings.HasPrefix(filepath.Base(path), ".") {
		t.Fatalf("backup path = %q, want visible .db in %q", path, backupDirectory)
	}
	secondPath, err := store.CreateBackup(ctx, backupDirectory)
	if err != nil {
		t.Fatalf("second CreateBackup(): %v", err)
	}
	if secondPath == path {
		t.Fatalf("rapid backups reused final path %q", path)
	}
	assertPrivateMode(t, backupDirectory, 0o700)
	assertPrivateMode(t, path, 0o600)
	assertPrivateMode(t, secondPath, 0o600)
	assertNoBackupTemporaryFiles(t, backupDirectory)
	if err := quickCheckSQLiteFile(ctx, path); err != nil {
		t.Fatalf("quickCheckSQLiteFile(%q): %v", path, err)
	}

	backup, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	queryErr := backup.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key = ?", "backup-fixture").Scan(&stored)
	_, writeErr := backup.ExecContext(ctx, "CREATE TABLE backup_must_be_read_only(value TEXT)")
	closeErr := backup.Close()
	if queryErr != nil {
		t.Fatalf("read backup snapshot: %v", queryErr)
	}
	if closeErr != nil {
		t.Fatalf("close backup snapshot: %v", closeErr)
	}
	if writeErr == nil {
		t.Fatal("read-only backup connection accepted a schema write")
	}
	if stored != `{"value":"durable snapshot"}` {
		t.Fatalf("backup setting = %s", stored)
	}
}

func TestCreateBackupCleansTemporaryFileOnCancellationAndVerificationFailure(t *testing.T) {
	store := openTestStore(t)
	directory := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("cancelled before vacuum", func(t *testing.T) {
		temporaryPath := filepath.Join(directory, ".cancelled.tmp")
		finalPath := filepath.Join(directory, "cancelled.db")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := store.createBackupAt(ctx, temporaryPath, finalPath, quickCheckSQLiteFile)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("createBackupAt() error = %v, want context cancellation", err)
		}
		assertPathMissing(t, temporaryPath)
		assertPathMissing(t, finalPath)
	})

	t.Run("verification fails after vacuum", func(t *testing.T) {
		temporaryPath := filepath.Join(directory, ".verification-failure.tmp")
		finalPath := filepath.Join(directory, "verification-failure.db")
		verifyErr := errors.New("injected quick_check failure")
		err := store.createBackupAt(context.Background(), temporaryPath, finalPath,
			func(context.Context, string) error { return verifyErr })
		if !errors.Is(err, verifyErr) {
			t.Fatalf("createBackupAt() error = %v, want %v", err, verifyErr)
		}
		assertPathMissing(t, temporaryPath)
		assertPathMissing(t, finalPath)
	})
}

func TestCreateBackupNeverReplacesExistingFinalFile(t *testing.T) {
	store := openTestStore(t)
	directory := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	temporaryPath := filepath.Join(directory, ".collision.tmp")
	finalPath := filepath.Join(directory, "collision.db")
	want := []byte("existing backup must survive")
	if err := os.WriteFile(finalPath, want, 0o600); err != nil {
		t.Fatal(err)
	}

	err := store.createBackupAt(context.Background(), temporaryPath, finalPath, quickCheckSQLiteFile)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("createBackupAt() error = %v, want fs.ErrExist", err)
	}
	got, readErr := os.ReadFile(finalPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(want) {
		t.Fatalf("existing final contents = %q, want %q", got, want)
	}
	assertPathMissing(t, temporaryPath)
}

func assertPrivateMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != want {
		t.Fatalf("mode for %q = %04o, want %04o", path, info.Mode().Perm(), want)
	}
}

func assertNoBackupTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || filepath.Ext(entry.Name()) == ".tmp" {
			t.Errorf("temporary backup was not removed: %s", entry.Name())
		}
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("path %q still exists: %v", path, err)
	}
}
