package persistence

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/veqri/veqri/internal/securefs"
)

// CreateBackup writes a verified, crash-durable SQLite snapshot into directory.
// The final .db name is not published until VACUUM INTO has completed, the
// snapshot passes SQLite quick_check, and the temporary file has been synced.
func (s *Store) CreateBackup(ctx context.Context, directory string) (string, error) {
	if err := securefs.EnsurePrivateDirDurable(directory); err != nil {
		return "", fmt.Errorf("secure backup directory: %w", err)
	}

	suffix, err := backupSuffix()
	if err != nil {
		return "", err
	}
	stem := "veqri-" + time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + suffix
	temporaryPath := filepath.Join(directory, "."+stem+".tmp")
	finalPath := filepath.Join(directory, stem+".db")
	if err := s.createBackupAt(ctx, temporaryPath, finalPath, quickCheckSQLiteFile); err != nil {
		return "", err
	}
	return finalPath, nil
}

func (s *Store) createBackupAt(ctx context.Context, temporaryPath, finalPath string,
	verify func(context.Context, string) error) error {
	if filepath.Dir(temporaryPath) != filepath.Dir(finalPath) {
		return errors.New("backup temporary and final files must share a directory")
	}
	if filepath.Ext(temporaryPath) == ".db" || !strings.HasPrefix(filepath.Base(temporaryPath), ".") {
		return errors.New("backup temporary file must be hidden and must not use the .db extension")
	}
	if verify == nil {
		return errors.New("backup verifier is required")
	}
	if err := securefs.EnsurePrivateDir(filepath.Dir(finalPath)); err != nil {
		return fmt.Errorf("secure backup directory: %w", err)
	}
	for label, path := range map[string]string{"temporary": temporaryPath, "final": finalPath} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%s backup path %q: %w", label, path, fs.ErrExist)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect %s backup path %q: %w", label, path, err)
		}
	}

	published := false
	defer func() {
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()

	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", temporaryPath); err != nil {
		return fmt.Errorf("create SQLite backup: %w", err)
	}
	if err := securefs.EnsurePrivateFile(temporaryPath); err != nil {
		return fmt.Errorf("secure temporary backup: %w", err)
	}
	if err := verify(ctx, temporaryPath); err != nil {
		return fmt.Errorf("verify SQLite backup: %w", err)
	}
	if err := syncBackupFile(temporaryPath); err != nil {
		return fmt.Errorf("sync SQLite backup: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("publish SQLite backup: %w", err)
	}
	// The generated final name is random and the private directory is not
	// writable by other users. Refuse an unexpected collision before rename so
	// an existing backup is never intentionally replaced.
	if _, err := os.Lstat(finalPath); err == nil {
		return fmt.Errorf("final backup path %q: %w", finalPath, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect final backup path %q: %w", finalPath, err)
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return fmt.Errorf("publish SQLite backup: %w", err)
	}
	published = true
	if err := securefs.SyncDir(filepath.Dir(finalPath)); err != nil {
		return fmt.Errorf("sync backup directory: %w", err)
	}
	return nil
}

func backupSuffix() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate backup name: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func quickCheckSQLiteFile(ctx context.Context, path string) error {
	database, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return fmt.Errorf("open backup read-only: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	checkErr := runSQLiteQuickCheck(ctx, database)
	closeErr := database.Close()
	if checkErr != nil {
		return checkErr
	}
	if closeErr != nil {
		return fmt.Errorf("close verified backup: %w", closeErr)
	}
	return nil
}

func runSQLiteQuickCheck(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("run quick_check: %w", err)
	}
	defer rows.Close()
	checked := false
	for rows.Next() {
		checked = true
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("read quick_check result: %w", err)
		}
		if result != "ok" {
			return fmt.Errorf("quick_check failed: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read quick_check results: %w", err)
	}
	if !checked {
		return errors.New("quick_check returned no result")
	}
	return nil
}

func sqliteReadOnlyDSN(path string) string {
	uriPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && filepath.VolumeName(path) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	value := url.URL{Scheme: "file", Path: uriPath}
	query := value.Query()
	query.Set("mode", "ro")
	value.RawQuery = query.Encode()
	return value.String()
}

func syncBackupFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
