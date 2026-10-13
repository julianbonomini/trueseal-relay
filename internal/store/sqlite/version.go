package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
)

// CurrentStoreVersion is the Store Version this binary writes (ADR-0013).
// Version 1 is the floor: the first preview release records it, and every
// later version migrates forward from it in one-transaction steps.
const CurrentStoreVersion = 1

// storeResetDeclared is "true" only in a build that declares itself a relay
// Store Reset release (ADR-0013). Set at link time:
//
//	-ldflags "-X github.com/julianbonomini/trueseal-relay/internal/store/sqlite.storeResetDeclared=true"
var storeResetDeclared = "false"

// StoreResetDeclared reports whether this binary is a declared relay Store
// Reset release. Only then may the operator's -reset-store flag be honoured.
func StoreResetDeclared() bool { return storeResetDeclared == "true" }

// Options controls the destructive paths of the versioned open.
type Options struct {
	// ResetRequested is the operator's -reset-store flag.
	ResetRequested bool
	// ResetDeclared reports whether this binary is a declared Store Reset
	// release — pass StoreResetDeclared() from main. (A field rather than a
	// direct read so tests can exercise both release kinds.)
	ResetDeclared bool
}

// TooNewError: the store was written by a newer release. The store is left
// byte-for-byte unchanged (the version is probed read-only).
type TooNewError struct{ Found, Supported int }

func (e *TooNewError) Error() string {
	return fmt.Sprintf("inbox store version %d is newer than this relay supports (store version %d) — refusing to start, store left unchanged; upgrade the relay", e.Found, e.Supported)
}

// UnmigratableError: the store is older or malformed and this binary has no
// migration path for it. ResetAvailable reports whether this is a declared
// Store Reset release — only then does the message name the reset flag.
type UnmigratableError struct {
	Found, Supported int
	ResetAvailable   bool
}

func (e *UnmigratableError) Error() string {
	base := fmt.Sprintf("inbox store version %d cannot be migrated to this relay's store version %d", e.Found, e.Supported)
	if e.ResetAvailable {
		return base + " — pass -reset-store to drop ALL inboxes and start fresh (operator opt-in per ADR-0013)"
	}
	return base + " — refusing to start, store left unchanged; this release is not a declared Store Reset release, see the upgrade docs"
}

// ResetNotDeclaredError: -reset-store was passed to a release that is not a
// declared Store Reset release. Refused before the store is touched.
type ResetNotDeclaredError struct{}

func (e *ResetNotDeclaredError) Error() string {
	return "-reset-store refused: this release is not a declared relay Store Reset release (ADR-0013) — the flag is honoured only by releases that declare one in their release notes"
}

// migration is one step of the forward chain: it takes a store at version
// `from` to version `from+1`. All steps of a chain run inside a single
// transaction with the version stamp (ADR-0013), so steps must not commit
// anything of their own.
type migration struct {
	from  int
	apply func(tx *sql.Tx) error
}

// migrations is the production forward chain. Empty at the v1 floor; future
// releases append {from: 1, ...} and bump CurrentStoreVersion.
var migrations = []migration{}

// probeResult describes an existing store file, read without modifying it.
type probeResult struct {
	exists        bool
	version       int
	hasInbox      bool
	schemaMatches bool
}

// probe reads the store version and shape through a read-only connection, so
// a refusal leaves the file byte-for-byte unchanged (ADR-0013).
func probe(path string) (probeResult, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return probeResult{}, nil
	} else if err != nil {
		return probeResult{}, fmt.Errorf("sqlite: stat store: %w", err)
	}

	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return probeResult{}, fmt.Errorf("sqlite: probe open: %w", err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return probeResult{}, fmt.Errorf("sqlite: probe version: %w", err)
	}

	var hasInbox bool
	if err := db.QueryRow(
		`SELECT COUNT(*) > 0 FROM sqlite_master WHERE type = 'table' AND name = 'inbox'`,
	).Scan(&hasInbox); err != nil {
		return probeResult{}, fmt.Errorf("sqlite: probe tables: %w", err)
	}

	res := probeResult{exists: true, version: version, hasInbox: hasInbox}
	if hasInbox {
		matches, err := inboxSchemaMatches(db)
		if err != nil {
			return probeResult{}, err
		}
		res.schemaMatches = matches
	}
	return res, nil
}

// inboxSchemaMatches reports whether the inbox table has exactly the columns
// the current schema defines. It is the adoption test for stores written
// before versioning existed (ADR-0013: "adopt them as version 1 when the
// schema matches, and otherwise require the reset flag").
func inboxSchemaMatches(db *sql.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(inbox)`)
	if err != nil {
		return false, fmt.Errorf("sqlite: probe schema: %w", err)
	}
	defer rows.Close()

	want := map[string]bool{"id": true, "recipient": true, "envelope": true, "expires_at": true}
	seen := 0
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("sqlite: probe schema scan: %w", err)
		}
		if !want[name] {
			return false, nil // unexpected column — not today's shape
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("sqlite: probe schema rows: %w", err)
	}
	return seen == len(want), nil
}

// openStore opens (creating, adopting, migrating or resetting) the store at
// path to the target version. Production calls it via New/NewWithOptions;
// the white-box tests call it with synthetic chains.
func openStore(path string, opts Options, chain []migration, target int) (*Store, error) {
	// A non-declared release refuses the flag before touching anything.
	if opts.ResetRequested && !opts.ResetDeclared {
		return nil, &ResetNotDeclaredError{}
	}

	p, err := probe(path)
	if err != nil {
		return nil, err
	}

	// A newer store is refused before ANYTHING else — including a declared
	// Store Reset release with the operator's flag. TooNew exists to protect
	// data written by a future release; letting -reset-store win here would
	// turn a downgrade-with-reset into silent destruction of that data
	// (orchestrator decision, 2026-10-13).
	if p.exists && p.version > target {
		return nil, &TooNewError{Found: p.version, Supported: target}
	}

	// A declared release with the operator's flag resets, whatever the
	// store's state — that is the flag's whole meaning (ADR-0013).
	if p.exists && opts.ResetDeclared && opts.ResetRequested {
		return resetStore(path, target)
	}

	switch {
	case !p.exists, p.version == 0 && !p.hasInbox:
		// Fresh — includes an empty file left by a crash before first write.
		return createFresh(path, target)
	case p.version == 0 && p.schemaMatches:
		// Today's unversioned store: adopt it, data untouched.
		return adoptUnversioned(path, target)
	case p.version == 0:
		return nil, &UnmigratableError{Found: 0, Supported: target, ResetAvailable: opts.ResetDeclared}
	case p.version == target:
		return openCurrent(path, target)
	default:
		return migrateForward(path, p.version, target, chain, opts)
	}
}

// openRW opens the store read-write with the crash-safe pragmas. Schema and
// version decisions belong to the callers above.
func openRW(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: set WAL mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=FULL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: set synchronous=FULL: %w", err)
	}
	return db, nil
}

// withTx runs fn in one transaction — the unit of atomicity for every
// versioned change. A crash inside fn leaves the old store intact.
func withTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// stamp sets PRAGMA user_version — inside the caller's transaction, so the
// version and the schema change commit or roll back together.
func stamp(tx *sql.Tx, version int) error {
	// user_version takes no bind parameter; version is an int we control.
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		return fmt.Errorf("sqlite: stamp version %d: %w", version, err)
	}
	return nil
}

func createFresh(path string, target int) (*Store, error) {
	db, err := openRW(path)
	if err != nil {
		return nil, err
	}
	if err := withTx(db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("sqlite: apply schema: %w", err)
		}
		return stamp(tx, target)
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, version: target}, nil
}

// adoptUnversioned stamps a schema-matching unversioned store as the current
// version without touching its rows (ADR-0013).
func adoptUnversioned(path string, target int) (*Store, error) {
	db, err := openRW(path)
	if err != nil {
		return nil, err
	}
	if err := withTx(db, func(tx *sql.Tx) error {
		return stamp(tx, target)
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, version: target}, nil
}

// openCurrent opens a store already at the target version. The schema apply
// stays idempotent, exactly as the unversioned New behaved.
func openCurrent(path string, target int) (*Store, error) {
	db, err := openRW(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: apply schema: %w", err)
	}
	return &Store{db: db, version: target}, nil
}

// migrateForward runs the whole chain in ONE transaction (ADR-0013: "an
// ordered chain in one transaction"). A crash or a failed step leaves the
// OLD store fully intact — version and schema roll back together — and the
// next start retries the chain from the beginning.
func migrateForward(path string, from, target int, chain []migration, opts Options) (*Store, error) {
	// Find the path first so a missing step is named before anything writes.
	steps := make([]*migration, 0, target-from)
	for v := from; v < target; v++ {
		var step *migration
		for i := range chain {
			if chain[i].from == v {
				step = &chain[i]
				break
			}
		}
		if step == nil {
			return nil, &UnmigratableError{Found: v, Supported: target, ResetAvailable: opts.ResetDeclared}
		}
		steps = append(steps, step)
	}

	db, err := openRW(path)
	if err != nil {
		return nil, err
	}
	if err := withTx(db, func(tx *sql.Tx) error {
		for i, step := range steps {
			if err := step.apply(tx); err != nil {
				return fmt.Errorf("sqlite: migrate store %d → %d: %w", from+i, from+i+1, err)
			}
		}
		return stamp(tx, target)
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, version: target}, nil
}

// resetStore drops every inbox and rebuilds at the target version. Only
// reachable on a declared Store Reset release with the operator's flag
// (ADR-0013). The keypair is a separate file and is never opened here.
func resetStore(path string, target int) (*Store, error) {
	db, err := openRW(path)
	if err != nil {
		return nil, err
	}
	if err := withTx(db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DROP TABLE IF EXISTS inbox`); err != nil {
			return fmt.Errorf("sqlite: reset drop inbox: %w", err)
		}
		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("sqlite: reset apply schema: %w", err)
		}
		return stamp(tx, target)
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, version: target, wasReset: true}, nil
}
