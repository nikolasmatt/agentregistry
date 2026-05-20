// Package migrate exposes the `arctl db migrate` subcommand and a
// package-private registry that lets the binary stack one or more
// migration sources behind a single CLI. Downstream distributions
// register additional sources alongside the OSS one via Register.
//
// Each source owns its own schema_migrations_<name> table (via
// golang-migrate's per-instance MigrationsTable), so adding a source
// never moves the OSS source's integer counter — the addressing
// footgun documented in the upstream ADR is structurally gone.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"sync"

	"github.com/golang-migrate/migrate/v4"
)

// sourceNameRE constrains Source.Name to Postgres-identifier-friendly
// characters. Name flows into the per-source schema_migrations_<name>
// table that go-migrate's MigrationsTable points at, so it must be a
// valid SQL identifier; we tighten further to lowercase + digits +
// underscore to keep operator-facing strings predictable.
var sourceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Source describes one set of migrations registered with the CLI.
type Source struct {
	// Name is the operator-visible label (e.g. "oss"). Surfaced by
	// `--source <name>` and by the per-source breakdown printed by
	// `status` and `version` when more than one source is registered.
	Name string

	// NewMigrator constructs a fresh *migrate.Migrate bound to dsn.
	// The CLI is responsible for calling mg.Close() after each call;
	// each invocation gets its own dedicated DB connection because
	// go-migrate's advisory lock is session-level. ctx applies to
	// any setup work (legacy bootstrap, advisory-lock acquisition);
	// go-migrate's own API is synchronous from the returned handle
	// onward.
	NewMigrator func(ctx context.Context, dsn string) (*migrate.Migrate, error)

	// Files is the embedded migration set. Exposed so the CLI can
	// walk it for pending-count math without pgring at the *migrate.Migrate
	// internals.
	Files fs.FS

	// Dir is the directory inside Files holding NNN_name.up.sql /
	// NNN_name.down.sql pairs.
	Dir string
}

var (
	sourcesMu sync.RWMutex
	sources   []Source
)

// Register adds a migration source to the package registry. Intended
// to be called from init() in the binary's root command package.
//
// Validates Name against ^[a-z][a-z0-9_]*$ — Name flows into the
// schema_migrations_<name> bookkeeping table that go-migrate's
// MigrationsTable points at, so it must be a valid SQL identifier.
// Lowercase + digits + underscore keeps operator-facing strings
// predictable across `--source` output and the breakdown lines that
// `status` / `version` print.
//
// Panics on duplicate Name: each source's Name maps 1:1 to its
// per-source schema_migrations_<name> table, so a duplicate would
// collide at the bookkeeping layer.
//
// Both checks panic rather than return an error because Register is
// expected to fire from init() — failing fast at process start makes
// the misconfiguration obvious.
//
// The mutex is defense-in-depth so a contract-violating caller
// running Register outside init() doesn't trigger a silent data race
// against Sources().
func Register(s Source) {
	if !sourceNameRE.MatchString(s.Name) {
		panic(fmt.Sprintf("migrate.Register: source Name=%q must match %s (Name embeds into the schema_migrations_<name> bookkeeping table and must be a valid SQL identifier)", s.Name, sourceNameRE.String()))
	}
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	for _, existing := range sources {
		if existing.Name == s.Name {
			panic(fmt.Sprintf("migrate.Register: source %q already registered; each source must have a unique Name (maps to a distinct schema_migrations_<name> table)", s.Name))
		}
	}
	sources = append(sources, s)
}

// Sources returns a copy of the registered sources in registration
// order. Returning a copy prevents callers from holding a reference
// that could race with a subsequent Register call.
func Sources() []Source {
	sourcesMu.RLock()
	defer sourcesMu.RUnlock()
	out := make([]Source, len(sources))
	copy(out, sources)
	return out
}

// ResetForTesting clears the registry. Test-only.
func ResetForTesting() {
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	sources = nil
}
