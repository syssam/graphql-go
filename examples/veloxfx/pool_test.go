package veloxfx

import (
	"context"
	"database/sql"
	"testing"
)

// A burst of concurrent requests must leave its connections in the pool for
// the next one. database/sql's default keeps two idle, closes the rest, and
// the next burst dials and authenticates each again -- on PostgreSQL that
// was 9% of a replica's CPU. MaxIdleClosed counts exactly those closes.
func TestPoolKeepsABurstsConnections(t *testing.T) {
	burst := func(db *sql.DB) sql.DBStats {
		t.Helper()
		ctx := context.Background()
		var conns []*sql.Conn
		for range 10 {
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
		}
		for _, c := range conns {
			c.Close()
		}
		return db.Stats()
	}
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}

	if s := burst(open()); s.MaxIdleClosed == 0 {
		t.Fatal("the default pool kept every connection; the comparison below would prove nothing")
	}
	db := open()
	configurePool(db, 0)
	if s := burst(db); s.MaxIdleClosed != 0 || s.Idle != 10 {
		t.Errorf("a configured pool closed %d of a burst's connections and kept %d idle, want 0 and 10", s.MaxIdleClosed, s.Idle)
	}
	if s := db.Stats(); s.MaxOpenConnections != defaultMaxConns {
		t.Errorf("MaxOpenConnections = %d, want %d", s.MaxOpenConnections, defaultMaxConns)
	}
}
