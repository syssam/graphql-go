package veloxfx

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/syssam/graphql-go/examples/veloxfx/velox"
)

// The suite runs on SQLite by default. With VELOXFX_POSTGRES set to a
// server's URL it runs on PostgreSQL, each test in a database of its own,
// which is where projection, per-parent limits and the ownership filter meet
// a planner that is not SQLite's:
//
//	VELOXFX_POSTGRES=postgres://postgres:pass@localhost:5432/postgres?sslmode=disable go test ./...
var postgresURL = os.Getenv("VELOXFX_POSTGRES")

var databases sync.Map // test name -> [2]string{driver, dsn}

// database returns the driver and data source of t's own database, the same
// on every call within t.
func database(t testing.TB) (driver, dsn string) {
	t.Helper()
	if v, ok := databases.Load(t.Name()); ok {
		d := v.([2]string)
		return d[0], d[1]
	}
	if postgresURL == "" {
		driver, dsn = "sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)"
	} else {
		driver, dsn = "postgres", postgresDatabase(t)
	}
	databases.Store(t.Name(), [2]string{driver, dsn})
	return driver, dsn
}

// postgresDatabase creates a database for t and drops it when t ends; it is
// registered before the app is started, so it is dropped after the app stops.
func postgresDatabase(t testing.TB) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.Name()))
	name := "veloxfx_" + hex.EncodeToString(sum[:8])
	admin, err := sql.Open("postgres", postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + name + " WITH (FORCE)", "CREATE DATABASE " + name} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		admin, err := sql.Open("postgres", postgresURL)
		if err != nil {
			return
		}
		defer admin.Close()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		databases.Delete(t.Name())
	})
	u, err := url.Parse(postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// testConfig is the app's Config for t: a free port and t's own database.
func testConfig(t testing.TB) Config {
	driver, dsn := database(t)
	return Config{Addr: "127.0.0.1:0", Driver: driver, DSN: dsn}
}

// openDB opens another client on t's database, as a test that decorates the
// app's client does.
func openDB(t testing.TB, opts ...velox.Option) (*velox.Client, error) {
	driver, dsn := database(t)
	return velox.Open(driver, dsn, opts...)
}
