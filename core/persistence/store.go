package persistence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const timestampFormat = time.RFC3339Nano

type Store struct {
	db *sql.DB
}

type embeddedMigration struct {
	version  int
	name     string
	contents []byte
	checksum string
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite PRAGMAs are connection-local. A single pooled connection keeps
	// foreign-key, busy-timeout, and WAL behavior consistent while task/tool
	// execution itself remains concurrent outside database transactions.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure sqlite (%s): %w", pragma, err)
		}
	}
	if err := store.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("sqlite quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("sqlite integrity check failed: %s", result)
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL, checksum TEXT NOT NULL DEFAULT '')`); err != nil {
		return fmt.Errorf("bootstrap migrations: %w", err)
	}
	if err := s.ensureMigrationChecksumColumn(ctx); err != nil {
		return err
	}
	var migrations []embeddedMigration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return fmt.Errorf("migration %q has no numeric prefix", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("migration %q has invalid version: %w", entry.Name(), err)
		}
		contents, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %d: %w", version, err)
		}
		digest := sha256.Sum256(contents)
		migrations = append(migrations, embeddedMigration{version: version, name: entry.Name(),
			contents: contents, checksum: hex.EncodeToString(digest[:])})
	}
	if len(migrations) == 0 {
		return errors.New("no embedded database migrations")
	}
	var newestApplied sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&newestApplied); err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	if newestApplied.Valid && newestApplied.Int64 > int64(migrations[len(migrations)-1].version) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)",
			newestApplied.Int64, migrations[len(migrations)-1].version)
	}
	for _, migration := range migrations {
		var recordedChecksum string
		err := s.db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version = ?", migration.version).
			Scan(&recordedChecksum)
		if err == nil {
			if recordedChecksum == "" {
				if _, err := s.db.ExecContext(ctx, "UPDATE schema_migrations SET checksum = ? WHERE version = ? AND checksum = ''",
					migration.checksum, migration.version); err != nil {
					return fmt.Errorf("backfill migration %d checksum: %w", migration.version, err)
				}
			} else if recordedChecksum != migration.checksum {
				return fmt.Errorf("migration %d checksum mismatch; database or embedded migration was modified", migration.version)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %d: %w", migration.version, err)
		}
		if err := s.applyMigration(ctx, migration); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureMigrationChecksumColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(schema_migrations)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		found = found || name == "checksum"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE schema_migrations ADD COLUMN checksum TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("add migration checksum metadata: %w", err)
	}
	return nil
}

func (s *Store) DB() *sql.DB { return s.db }

func formatTime(value time.Time) string { return value.UTC().Format(timestampFormat) }

func optionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(timestampFormat, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse persisted timestamp %q: %w", value, err)
	}
	return parsed, nil
}

func parseOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
