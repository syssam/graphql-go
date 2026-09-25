package veloxfx

import (
	"context"
	"fmt"

	"go.uber.org/fx"
	_ "modernc.org/sqlite"

	"github.com/syssam/graphql-go/examples/veloxfx/velox"
)

// NewClient opens the database. The migration runs in OnStart rather than
// here because it does I/O, and a start hook is given a context that fx
// cancels at its start timeout where a constructor is given none.
func NewClient(lc fx.Lifecycle, cfg Config) (*velox.Client, error) {
	client, err := velox.Open("sqlite", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
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
