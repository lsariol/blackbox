package store

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" driver for database/sql, which goose uses
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationsTable is where goose records applied migrations, in the blackbox
// schema with everything else Blackbox owns.
const migrationsTable = "blackbox.goose_db_version"

// undefinedTable is Postgres's error code for a missing table.
const undefinedTable = "42P01"

// Migrate applies every pending migration, as blackbox_migrator. A Postgres
// advisory lock keeps two starts from migrating at once.
func Migrate(ctx context.Context, connString string, log *slog.Logger) error {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("database: BLACKBOX_DATABASE_MIGRATOR_URL isn't a valid connection string: %w", err)
	}
	defer db.Close()

	// goose writes its version table into the schema, so the schema must exist first.
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS blackbox"); err != nil {
		return fmt.Errorf("database: preparing migrations as BLACKBOX_DATABASE_MIGRATOR_URL: %w", err)
	}
	fsys, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithTableName(migrationsTable),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		return fmt.Errorf("database: preparing migrations: %w", err)
	}

	results, err := provider.Up(ctx)
	for _, r := range results {
		log.Info("migration applied", "migration", r.Source.Path, "took", r.Duration.String())
	}
	if err != nil {
		return fmt.Errorf("database: applying migrations: %w", err)
	}
	return nil
}

// SchemaVersion returns the database's migration version and the version this
// build needs.
func (s *Store) SchemaVersion(ctx context.Context) (have, want int64, err error) {
	want, err = latestMigration()
	if err != nil {
		return 0, 0, err
	}
	const query = `SELECT COALESCE(MAX(version_id), 0) FROM blackbox.goose_db_version WHERE is_applied`
	if err := s.pool.QueryRow(ctx, query).Scan(&have); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			return 0, want, nil
		}
		return 0, want, fmt.Errorf("database: reading the schema version: %w", err)
	}
	return have, want, nil
}

// CheckSchemaVersion refuses a database missing a migration this build needs.
// A newer database is fine: migrations only ever add.
func (s *Store) CheckSchemaVersion(ctx context.Context) error {
	have, want, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if have < want {
		return fmt.Errorf("database: the schema is at version %d, this Blackbox needs %d: blackbox-collector applies migrations at startup; check its log and BLACKBOX_DATABASE_MIGRATOR_URL", have, want)
	}
	return nil
}

func latestMigration() (int64, error) {
	names, err := fs.Glob(embeddedMigrations, "migrations/*.sql")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, name := range names {
		v, err := goose.NumericComponent(name)
		if err != nil {
			return 0, fmt.Errorf("migration %q: %w", name, err)
		}
		latest = max(latest, v)
	}
	return latest, nil
}

// Migration is one of this build's migrations and when it was applied.
type Migration struct {
	Version   int64
	Name      string
	AppliedAt *time.Time // nil while pending
}

// Migrations lists this build's migrations, oldest first, with the time each
// was applied. The reader may read the version table.
func (s *Store) Migrations(ctx context.Context) ([]Migration, error) {
	names, err := fs.Glob(embeddedMigrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	applied := make(map[int64]time.Time)
	rows, err := s.pool.Query(ctx, `SELECT version_id, tstamp FROM blackbox.goose_db_version WHERE is_applied AND version_id > 0`)
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != undefinedTable {
			return nil, fmt.Errorf("database: reading the migrations table: %w", err)
		}
	} else {
		var v int64
		var at time.Time
		_, err = pgx.ForEachRow(rows, []any{&v, &at}, func() error {
			applied[v] = at.UTC()
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("database: reading the migrations table: %w", err)
		}
	}

	var out []Migration
	for _, name := range names {
		v, err := goose.NumericComponent(name)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", name, err)
		}
		m := Migration{Version: v, Name: strings.TrimSuffix(path.Base(name), ".sql")}
		if at, ok := applied[v]; ok {
			m.AppliedAt = &at
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	return out, nil
}
