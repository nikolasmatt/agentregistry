# OSS migration set

This directory holds the v1alpha1 Postgres migrations applied by both
the server (at startup, unless `SkipMigrations` is set) and the
`arctl db migrate` CLI.

## File-naming convention

```
NNN_short_name.sql           # the forward migration (required)
NNN_short_name.down.sql      # the reverse migration (optional)
```

- `NNN` is the migration version, zero-padded to three digits. New
  migrations get the next unused number. Pre-offset numbers are kept
  small (the runtime `VersionOffset` is added when the migrator reads
  the file — see `pkg/registry/database/migrate.go`).
- `short_name` is a lowercase, underscore-separated description of the
  change. Used in slog output and in error messages — keep it
  readable.
- Same prefix on both files: `005_widget_table.sql` pairs with
  `005_widget_table.down.sql`. The loader pairs them by prefix; a
  `.down.sql` whose `.sql` partner is missing is ignored.

## Forward migrations

`NNN_short_name.sql` is the schema change. It runs inside a
transaction with the matching `schema_migrations` insert; either both
land or both don't.

Keep it idempotent-friendly when reasonable (`CREATE TABLE IF NOT
EXISTS`, `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`) so a half-applied
state can be reconciled by re-running.

## Reverse migrations

`NNN_short_name.down.sql` is the inverse. It runs inside a transaction
with the matching `schema_migrations` delete on `arctl db migrate
down N` / `arctl db migrate goto V` (backward).

- If you can write a sensible reverse, do. New migrations should ship
  with a `.down.sql` so operators can roll back.
- If you genuinely can't (destructive forward, no reverse), omit the
  `.down.sql`. `arctl db migrate down` across that migration will
  fail with `ErrNotReversible`, naming the file — that's the
  expected failure mode for irreversible changes.
- Existing migrations `001–006` ship up-only for backward
  compatibility. Down support is going-forward-only: backfill is
  additive (drop a `.down.sql` next to the existing `.sql`) but not
  required.

## Skip-gated migrations

A migration may be omitted from the install set when a runtime
feature flag is off — `003_embeddings.sql` is gated on
`AGENT_REGISTRY_EMBEDDINGS_ENABLED` so vanilla installs without the
`pgvector` extension don't fail at boot. The gating predicate lives
in `pkg/registry/v1alpha1store/migrator.go` (`Skip`), not in the
filename. To gate a new migration, add to that predicate.

Operators toggling such a flag after applying the migration will see
"orphan" rows in `schema_migrations` (applied versions whose `.sql`
is no longer visible). `arctl db migrate down` / `force` surface a
hint pointing at the Skip predicate; restoring the configuration that
was in effect when the migration was applied makes the row
reachable again.

## Quick examples

A simple forward + reverse pair:

```
007_widget_owner_index.sql:
  CREATE INDEX IF NOT EXISTS widget_owner_idx ON widget (owner);

007_widget_owner_index.down.sql:
  DROP INDEX IF EXISTS widget_owner_idx;
```

A column add:

```
008_widget_archived.sql:
  ALTER TABLE widget ADD COLUMN archived BOOLEAN NOT NULL DEFAULT FALSE;

008_widget_archived.down.sql:
  ALTER TABLE widget DROP COLUMN IF EXISTS archived;
```

## Testing

Integration tests for the migrator live in
`pkg/registry/database/migrate_integration_test.go` (build tag
`integration`). They use the `testdata/integration_*` fixture
directories rather than this OSS set — the fixtures stay small and
focused on migrator behavior, not v1alpha1 semantics.

Run them via:

```
make test    # runs unit + integration against localhost:5432
```
