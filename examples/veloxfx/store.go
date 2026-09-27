package veloxfx

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
	"go.uber.org/fx"
	_ "modernc.org/sqlite"

	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	veloxsql "github.com/syssam/velox/dialect/sql"
)

// defaultMaxConns bounds the pool when Config.MaxConns is zero. A replica
// holds this many connections to the database at most, so it is sized
// against the database's own limit divided by the replica count, not against
// the request rate.
const defaultMaxConns = 20

// configurePool keeps as many connections idle as may be open. database/sql
// keeps two by default, so under load every request past the second opened a
// connection and closed it again: on PostgreSQL, whose SCRAM handshake hashes
// the password on every connect, that was 9% of a replica's CPU
// (BenchmarkOrderHistoryPageParallel). ConnMaxIdleTime still returns the
// pool to the database after a burst.
func configurePool(db *sql.DB, maxConns int) {
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
}

// NewClient opens the database. The migration runs in OnStart rather than
// here because it does I/O, and a start hook is given a context that fx
// cancels at its start timeout where a constructor is given none.
func NewClient(lc fx.Lifecycle, cfg Config, tr Tracing) (*velox.Client, error) {
	driver := cfg.Driver
	if driver == "" {
		driver = "sqlite"
	}
	db, err := openSQL(driver, cfg.DSN, tr)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	configurePool(db, cfg.MaxConns)
	client := velox.NewClient(velox.Driver(veloxsql.OpenDB(driver, db)))
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := client.Schema.Create(ctx); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
			return nil
		},
		OnStop: func(context.Context) error { return client.Close() },
	})
	return client, nil
}
