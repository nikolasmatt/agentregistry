package v1alpha1store_test

import (
	"bufio"
	"bytes"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentregistry-dev/agentregistry/pkg/registry/v1alpha1store"
)

// TestMigrationsLint walks the embedded migration set and rejects
// patterns that pin migrations to a specific Postgres schema. The
// runtime schema is set via migratepgx.Config{SchemaName: ...} and
// resolved through search_path, so every identifier in a migration
// must stay unqualified.
//
// Failing patterns surface as `filename:line — match` so authors land
// straight on the offending line.
func TestMigrationsLint(t *testing.T) {
	violations := lintMigrations(t, v1alpha1store.MigrationFiles, v1alpha1store.MigrationsDir)
	for _, v := range violations {
		t.Errorf("%s", v)
	}
}

// lintMigrations is factored out so the negative test below can
// reach in and exercise it with a fixture FS without touching the
// real embed.
func lintMigrations(t *testing.T, files fs.FS, dir string) []string {
	t.Helper()
	var violations []string
	err := fs.WalkDir(files, dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".sql") {
			return nil
		}
		data, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		violations = append(violations, scanSQL(path, data)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk migrations FS: %v", err)
	}
	return violations
}

// qualifiedIdent matches a schema-qualified identifier in either bare
// or double-quoted form: word.word, "word".word, word."word", or
// "word"."word". Spaces around the dot are tolerated (Postgres accepts
// them).
const qualifiedIdent = `(?:\w+|"[^"]+")\s*\.\s*(?:\w+|"[^"]+")`

// forbiddenPatterns is the catalogue of rejected substrings. Each
// entry pairs a regex with a human-readable explanation surfaced when
// a migration trips the lint.
var forbiddenPatterns = []struct {
	re      *regexp.Regexp
	message string
}{
	{
		// Match either bare `v1alpha1.` or quoted `"v1alpha1".` and
		// the same for the other named schemas.
		re:      regexp.MustCompile(`(?:\b(?:v1alpha1|public|agentregistry|pg_catalog|information_schema)\b|"(?:v1alpha1|public|agentregistry|pg_catalog|information_schema)")\s*\.`),
		message: "schema-qualified identifier (drop the prefix; the driver sets search_path)",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCREATE\s+SCHEMA\b`),
		message: "CREATE SCHEMA is not allowed in migrations (the orchestrator creates the schema before applying the migration)",
	},
	{
		re:      regexp.MustCompile(`(?i)\bSET\s+search_path\b`),
		message: "SET search_path is not allowed in migrations (the driver sets it)",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?\s+` + qualifiedIdent),
		message: "CREATE TABLE in a non-default schema is not allowed (the CREATE TABLE x.y rule catches arbitrary downstream schemas without naming them)",
	},
	{
		re:      regexp.MustCompile(`(?i)\bALTER\s+TABLE(?:\s+IF\s+EXISTS)?\s+(?:ONLY\s+)?` + qualifiedIdent),
		message: "ALTER TABLE addressing a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCREATE(?:\s+UNIQUE)?\s+INDEX(?:\s+CONCURRENTLY)?(?:\s+IF\s+NOT\s+EXISTS)?\s+(?:\w+|"[^"]+")\s+ON\s+` + qualifiedIdent),
		message: "CREATE INDEX targeting a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCREATE(?:\s+OR\s+REPLACE)?\s+VIEW(?:\s+IF\s+NOT\s+EXISTS)?\s+` + qualifiedIdent),
		message: "CREATE VIEW in a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCREATE\s+SEQUENCE(?:\s+IF\s+NOT\s+EXISTS)?\s+` + qualifiedIdent),
		message: "CREATE SEQUENCE in a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCREATE\s+TYPE\s+` + qualifiedIdent),
		message: "CREATE TYPE in a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bALTER\s+(?:INDEX|SEQUENCE|TYPE|VIEW|FUNCTION)(?:\s+IF\s+EXISTS)?\s+` + qualifiedIdent),
		message: "ALTER addressing a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCOMMENT\s+ON\s+\w+\s+` + qualifiedIdent),
		message: "COMMENT ON targeting a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\b(?:GRANT|REVOKE)\b.*?\bON\s+` + qualifiedIdent),
		message: "GRANT/REVOKE targeting a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCREATE(?:\s+OR\s+REPLACE)?\s+TRIGGER\s+(?:\w+|"[^"]+")\s+(?:BEFORE|AFTER|INSTEAD\s+OF)\b.*?\bON\s+` + qualifiedIdent),
		message: "CREATE TRIGGER targeting a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bCREATE(?:\s+OR\s+REPLACE)?\s+FUNCTION\s+` + qualifiedIdent + `\s*\(`),
		message: "CREATE FUNCTION in a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bINSERT\s+INTO\s+` + qualifiedIdent),
		message: "INSERT INTO addressing a non-default schema is not allowed",
	},
	{
		re:      regexp.MustCompile(`(?i)\bDROP\s+(?:TABLE|INDEX|TRIGGER|FUNCTION)(?:\s+IF\s+EXISTS)?\s+` + qualifiedIdent),
		message: "DROP addressing a non-default schema is not allowed",
	},
	{
		// CONCURRENTLY (CREATE/DROP INDEX, REINDEX) cannot run inside a
		// transaction block. The driver wraps each migration file in one
		// implicit transaction; a CONCURRENTLY statement would either error
		// or, on its own, apply non-atomically — breaking the
		// single-transaction invariant the orchestrator's failure-restore
		// relies on (a failed file must roll back its DDL, leaving only a
		// dirty marker, never half-applied schema).
		re:      regexp.MustCompile(`(?i)\bCONCURRENTLY\b`),
		message: "CONCURRENTLY is not allowed (it breaks the single-transaction atomicity the orchestrator's failure-restore depends on)",
	},
	{
		// Explicit COMMIT/ROLLBACK ends the implicit transaction early, so
		// statements after it apply non-atomically. Anchored to statement
		// forms (`COMMIT;`, `COMMIT WORK`, end-of-line) so the words inside
		// string literals or identifiers like `commit_sha` don't trip it.
		re:      regexp.MustCompile(`(?i)\b(?:COMMIT|ROLLBACK)\b\s*(?:;|WORK\b|TRANSACTION\b|AND\b|$)`),
		message: "explicit COMMIT/ROLLBACK is not allowed (it breaks the single-transaction atomicity the orchestrator's failure-restore depends on)",
	},
	{
		// Explicit transaction openers. PL/pgSQL block `BEGIN` (as in
		// `DO $$ BEGIN ... END $$`) is never written `BEGIN;` / `BEGIN
		// TRANSACTION` / `BEGIN WORK`, so only the transaction-control forms
		// match.
		re:      regexp.MustCompile(`(?i)(?:\bSTART\s+TRANSACTION\b|\bBEGIN\s+TRANSACTION\b|\bBEGIN\s+WORK\b|\bBEGIN\s*;)`),
		message: "explicit transaction control (BEGIN/START TRANSACTION) is not allowed (the driver wraps each migration file in one implicit transaction)",
	},
	{
		// Statements Postgres refuses to run inside a transaction block.
		// In a multi-statement file they fail loudly ("cannot run inside a
		// transaction block"); alone they apply non-atomically. Either way
		// they break the single-transaction guarantee the orchestrator's
		// failure-restore relies on, so reject them at lint time rather
		// than apply time. This is not exhaustive — any other
		// non-transactional statement is equally unsafe — but it catches
		// the ones most likely to be written.
		re:      regexp.MustCompile(`(?i)(?:\bVACUUM\b|\b(?:CREATE|DROP)\s+(?:DATABASE|TABLESPACE)\b|\bALTER\s+SYSTEM\b)`),
		message: "non-transactional statement (VACUUM / CREATE|DROP DATABASE|TABLESPACE / ALTER SYSTEM) is not allowed (it cannot run inside the single transaction the driver wraps each migration file in)",
	},
}

// scanSQL applies the forbidden-pattern catalogue to a single file.
// Returns one violation string per match.
func scanSQL(path string, data []byte) []string {
	var out []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		// Strip SQL line comments so commentary doesn't flag the lint.
		// String literals are NOT stripped, so a forbidden keyword inside
		// a string (e.g. 'ALTER SYSTEM disabled') would false-positive.
		// Accepted: migrations don't carry such literals, and a false
		// positive fails safe (rejects a migration that would have applied).
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		for _, p := range forbiddenPatterns {
			if loc := p.re.FindStringIndex(line); loc != nil {
				out = append(out, formatViolation(path, lineNum, line[loc[0]:loc[1]], p.message))
			}
		}
	}
	return out
}

func formatViolation(path string, line int, match, message string) string {
	return path + ":" + strconv.Itoa(line) + " — " + message + " (matched: " + match + ")"
}

// TestMigrationsLint_FlagsForbiddenPatterns asserts each category of
// forbidden pattern surfaces as a violation with a clear message.
// Catches regressions in the lint when the regex catalogue evolves.
func TestMigrationsLint_FlagsForbiddenPatterns(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"schema-qualified identifier",
			"CREATE TABLE IF NOT EXISTS v1alpha1.agents (id int);",
			"schema-qualified identifier",
		},
		{
			"CREATE SCHEMA",
			"CREATE SCHEMA agentregistry;",
			"CREATE SCHEMA is not allowed",
		},
		{
			"SET search_path",
			"SET search_path TO agentregistry;",
			"SET search_path is not allowed",
		},
		{
			"CREATE TABLE non-default schema",
			"CREATE TABLE other.foo (id int);",
			"CREATE TABLE in a non-default schema",
		},
		{
			"ALTER TABLE non-default schema",
			"ALTER TABLE other.foo ADD COLUMN bar int;",
			"ALTER TABLE addressing a non-default schema",
		},
		{
			"CREATE INDEX non-default schema",
			"CREATE INDEX foo_idx ON other.foo (id);",
			"CREATE INDEX targeting a non-default schema",
		},
		{
			"CREATE TRIGGER non-default schema",
			"CREATE TRIGGER foo_trg BEFORE UPDATE ON other.foo FOR EACH ROW EXECUTE FUNCTION noop();",
			"CREATE TRIGGER targeting a non-default schema",
		},
		{
			"CREATE FUNCTION non-default schema",
			"CREATE OR REPLACE FUNCTION other.fn() RETURNS void AS $$ BEGIN END; $$;",
			"CREATE FUNCTION in a non-default schema",
		},
		{
			"INSERT INTO non-default schema",
			"INSERT INTO other.foo (id) VALUES (1);",
			"INSERT INTO addressing a non-default schema",
		},
		{
			"DROP non-default schema",
			"DROP TABLE IF EXISTS other.foo;",
			"DROP addressing a non-default schema",
		},
		{
			"CREATE INDEX CONCURRENTLY non-default schema",
			"CREATE INDEX CONCURRENTLY foo_idx ON other.foo (id);",
			"CREATE INDEX targeting a non-default schema",
		},
		{
			"CREATE VIEW non-default schema",
			"CREATE VIEW other.foo AS SELECT 1;",
			"CREATE VIEW in a non-default schema",
		},
		{
			"CREATE SEQUENCE non-default schema",
			"CREATE SEQUENCE other.foo_seq;",
			"CREATE SEQUENCE in a non-default schema",
		},
		{
			"CREATE TYPE non-default schema",
			"CREATE TYPE other.foo_t AS ENUM ('a', 'b');",
			"CREATE TYPE in a non-default schema",
		},
		{
			"ALTER INDEX non-default schema",
			"ALTER INDEX other.foo_idx RENAME TO bar_idx;",
			"ALTER addressing a non-default schema",
		},
		{
			"COMMENT ON non-default schema",
			"COMMENT ON TABLE other.foo IS 'a foo';",
			"COMMENT ON targeting a non-default schema",
		},
		{
			"GRANT non-default schema",
			"GRANT SELECT ON other.foo TO public;",
			"GRANT/REVOKE targeting a non-default schema",
		},
		{
			"CREATE TABLE quoted identifiers",
			`CREATE TABLE "other"."foo" (id int);`,
			"CREATE TABLE in a non-default schema",
		},
		{
			"INSERT INTO quoted identifiers",
			`INSERT INTO "other"."foo" (id) VALUES (1);`,
			"INSERT INTO addressing a non-default schema",
		},
		{
			"CREATE INDEX CONCURRENTLY (unqualified)",
			"CREATE INDEX CONCURRENTLY foo_idx ON foo (id);",
			"CONCURRENTLY is not allowed",
		},
		{
			"explicit COMMIT",
			"CREATE TABLE foo (id int);\nCOMMIT;",
			"explicit COMMIT/ROLLBACK is not allowed",
		},
		{
			"explicit BEGIN;",
			"BEGIN;\nCREATE TABLE foo (id int);",
			"explicit transaction control",
		},
		{
			"explicit START TRANSACTION",
			"START TRANSACTION;\nCREATE TABLE foo (id int);",
			"explicit transaction control",
		},
		{
			"VACUUM",
			"VACUUM ANALYZE foo;",
			"non-transactional statement",
		},
		{
			"CREATE DATABASE",
			"CREATE DATABASE foo;",
			"non-transactional statement",
		},
		{
			"ALTER SYSTEM",
			"ALTER SYSTEM SET work_mem = '64MB';",
			"non-transactional statement",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := fstest.MapFS{
				"migrations/999_test.up.sql": &fstest.MapFile{Data: []byte(tc.body)},
			}
			violations := lintMigrations(t, fixture, "migrations")
			if len(violations) == 0 {
				t.Fatalf("expected at least one violation for body %q, got none", tc.body)
			}
			found := false
			for _, v := range violations {
				if strings.Contains(v, tc.want) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected violation containing %q; got %v", tc.want, violations)
			}
		})
	}
}

// TestMigrationsLint_AllowsPlpgsqlBlocks confirms the transaction-control
// rules don't false-positive on PL/pgSQL block syntax — `DO $$ BEGIN ...
// END $$`, `END IF;`, and a trigger function body — none of which are
// transaction control. The real `001` down file uses the DO/BEGIN/END
// form.
func TestMigrationsLint_AllowsPlpgsqlBlocks(t *testing.T) {
	bodies := []string{
		"DO $$ BEGIN\n  RAISE EXCEPTION 'not reversible';\nEND $$;",
		"CREATE OR REPLACE FUNCTION notify_fn() RETURNS trigger AS $$\nBEGIN\n  PERFORM pg_notify('agents_status', NEW.id::text);\n  RETURN NEW;\nEND;\n$$ LANGUAGE plpgsql;",
		"DO $$ BEGIN\n  IF NOT EXISTS (SELECT 1) THEN\n    NULL;\n  END IF;\nEND $$;",
		"INSERT INTO audit (action) VALUES ('commit');",
		"CREATE TABLE foo (commit_sha text);",
	}
	for _, body := range bodies {
		fixture := fstest.MapFS{
			"migrations/999_test.up.sql": &fstest.MapFile{Data: []byte(body)},
		}
		if violations := lintMigrations(t, fixture, "migrations"); len(violations) != 0 {
			t.Errorf("expected no violations for PL/pgSQL body %q; got %v", body, violations)
		}
	}
}

// TestMigrationsLint_IgnoresComments confirms the lint strips SQL line
// comments before matching — commentary like "removes the v1alpha1.
// prefix" must not trip the lint.
func TestMigrationsLint_IgnoresComments(t *testing.T) {
	fixture := fstest.MapFS{
		"migrations/999_test.up.sql": &fstest.MapFile{
			Data: []byte("-- this migration drops the v1alpha1. prefix\nCREATE TABLE agents (id int);\n"),
		},
	}
	violations := lintMigrations(t, fixture, "migrations")
	if len(violations) != 0 {
		t.Errorf("expected no violations on commentary-only mention; got %v", violations)
	}
}
