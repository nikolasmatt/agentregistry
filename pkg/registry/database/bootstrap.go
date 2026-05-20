package database

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx stdlib driver — required by sql.Open("pgx", ...)
)

// BootstrapLegacyOSSMigrations bridges OSS deployments that ran the
// pre-engine-swap custom migrator (`schema_migrations` with columns
// version/name/applied_at and a +200 version offset) to the
// golang-migrate shape (`schema_migrations` with version/dirty, where
// a single row records the highest applied version).
//
// On fresh installs and on deployments already bridged the function
// no-ops. On a legacy DB it runs once in a single transaction:
//
//   1. Renames the legacy table to schema_migrations_v0_legacy
//      (audit trail preserved).
//   2. Creates the new go-migrate-shaped schema_migrations table.
//   3. Inserts a single row whose version is the highest legacy OSS
//      version that (a) sits in [201, 499] AND (b) has a matching
//      NNN_*.up.sql in migrationsFS/dir, with the +200 offset
//      stripped. Orphan legacy rows (no matching .up.sql) are
//      skipped — the legacy table preserves them for forensics.
//
// Rows with `version >= 500` are intentionally left untouched in
// schema_migrations_v0_legacy for downstream extension bootstraps to
// claim — the independent-tracks split is layered, not bundled.
//
// The function is safe to call from multiple sources concurrently:
// the RENAME serializes via Postgres's ACCESS EXCLUSIVE lock, and a
// no-op outcome is the steady state once any caller has bridged.
func BootstrapLegacyOSSMigrations(ctx context.Context, dsn string, migrationsFS fs.FS, dir string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database for bootstrap: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Probe: does a legacy-shape schema_migrations table exist? The
	// `name` column was on the custom migrator but is absent from
	// go-migrate's schema, so its presence is the unambiguous
	// legacy-shape marker.
	var hasNameColumn bool
	probe := `
		SELECT EXISTS (
		    SELECT 1 FROM information_schema.columns
		    WHERE table_schema = 'public'
		      AND table_name = 'schema_migrations'
		      AND column_name = 'name'
		)`
	if err := db.QueryRowContext(ctx, probe).Scan(&hasNameColumn); err != nil {
		return fmt.Errorf("probe legacy schema_migrations: %w", err)
	}
	if !hasNameColumn {
		return nil
	}

	valid, err := loadValidUpVersions(migrationsFS, dir)
	if err != nil {
		return fmt.Errorf("scan migration sources: %w", err)
	}

	// Pre-fetch legacy OSS versions into memory before opening the
	// bootstrap tx; pgx's stdlib bridge doesn't allow interleaved
	// SELECT cursors and DML on the same tx-bound connection.
	legacyVersions, err := readLegacyOSSVersions(ctx, db)
	if err != nil {
		return err
	}

	maxValid, dropped := highestCarryable(legacyVersions, valid)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bootstrap tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE schema_migrations RENAME TO schema_migrations_v0_legacy`); err != nil {
		return fmt.Errorf("rename legacy schema_migrations: %w", err)
	}

	// Match go-migrate pgx/v5 driver's CREATE TABLE shape exactly so
	// it accepts the pre-created table without complaint.
	if _, err := tx.ExecContext(ctx,
		`CREATE TABLE schema_migrations (version BIGINT NOT NULL PRIMARY KEY, dirty BOOLEAN NOT NULL)`); err != nil {
		return fmt.Errorf("create new schema_migrations: %w", err)
	}

	if maxValid > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, dirty) VALUES ($1, false)`,
			maxValid); err != nil {
			return fmt.Errorf("insert bridged row v%d: %w", maxValid, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bootstrap tx: %w", err)
	}

	logger := slog.Default().With("component", "database.bootstrap")
	logger.Info("bridged legacy schema_migrations to go-migrate shape",
		"highest_carried", maxValid,
		"dropped", len(dropped))
	if len(dropped) > 0 {
		logger.Info("orphan legacy rows skipped during bridge (no matching .up.sql in current embed)",
			"versions", dropped)
	}
	return nil
}

// readLegacyOSSVersions reads every legacy schema_migrations.version
// in the OSS range [201, 499] into a slice. Done outside the bootstrap
// tx because pgx's stdlib bridge can't interleave a SELECT cursor and
// DML on the same tx-bound connection.
func readLegacyOSSVersions(ctx context.Context, db *sql.DB) ([]int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT version FROM schema_migrations WHERE version BETWEEN 201 AND 499 ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read legacy OSS rows: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan legacy version: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate legacy rows: %w", err)
	}
	return out, nil
}

// highestCarryable returns the highest (legacy_version - 200) that
// has a matching .up.sql in the valid set, plus the orphans that
// were skipped (legacy versions whose stripped form is not in valid).
// Returns 0 for the carried version when no legacy row qualifies.
func highestCarryable(legacyVersions []int, valid map[int]bool) (carried int, dropped []int) {
	for _, v := range legacyVersions {
		stripped := v - 200
		if !valid[stripped] {
			dropped = append(dropped, v)
			continue
		}
		if stripped > carried {
			carried = stripped
		}
	}
	return carried, dropped
}

// loadValidUpVersions scans migrationsFS/dir for NNN_name.up.sql
// files and returns a set of the parsed pre-offset version numbers.
// Used by the bootstrap to filter out legacy rows whose forward SQL
// is no longer in the embedded FS (e.g. a migration that the upstream
// codebase later replaced with a no-op or removed entirely).
func loadValidUpVersions(migrationsFS fs.FS, dir string) (map[int]bool, error) {
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return nil, err
	}
	valid := make(map[int]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		parts := strings.SplitN(name, "_", 2)
		if len(parts) != 2 {
			continue
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		valid[v] = true
	}
	return valid, nil
}
