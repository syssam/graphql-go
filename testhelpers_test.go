package graphql

import (
	"context"
	"strconv"
)

func ctxBackground() context.Context { return context.Background() }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
