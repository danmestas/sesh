package cli

import (
	"database/sql"
	"errors"
	"fmt"

	libfossildb "github.com/danmestas/libfossil/db"
)

// readVVarRepository opens dbPath (a .fslckout SQLite file) read-only
// and returns the 'repository' vvar row, which libfossil's checkout.Create
// stamps with the absolute path of the backing repo. Used by WorktreeCmd's
// idempotency probe to detect a checkout dir bound to a different repo
// than the one the operator asked for.
//
// Returns "" (no error) for the corner case of a .fslckout that exists
// but has no 'repository' vvar — treat as "unknown, can't verify, refuse
// to mutate" at the call site rather than fabricating a match.
//
// The SQL driver is libfossil's registered driver (modernc on non-wasm
// builds, ncruces on wasm). It's already registered process-wide via
// EdgeSync/hub's blank-import of db/driver/modernc — but to keep this
// file's intent obvious, we look up the driver by name from the libfossil
// package rather than assume the import chain.
func readVVarRepository(dbPath string) (string, error) {
	drv := libfossildb.RegisteredDriver()
	if drv == nil {
		return "", errors.New("no libfossil SQLite driver registered (import EdgeSync/hub or libfossil/db/driver/modernc)")
	}
	db, err := sql.Open(drv.Name, dbPath)
	if err != nil {
		return "", fmt.Errorf("open .fslckout %s: %w", dbPath, err)
	}
	defer db.Close()

	var val string
	err = db.QueryRow(`SELECT value FROM vvar WHERE name = 'repository'`).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query vvar.repository: %w", err)
	}
	return val, nil
}
