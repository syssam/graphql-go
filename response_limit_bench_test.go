package graphql

import "testing"

// The pair measures what a response limit costs on the write path: one atomic
// report per field and the sharing across concurrent sub-writers. The limit is
// far above the response, so both write identical bytes.
const responseLimitBenchQuery = `{ users { id name tags friends { id name tags friends { id name } } } }`

func BenchmarkResponseLimitOff(b *testing.B) {
	_, e := newFixtureExecutor(b)
	benchRun(b, e, responseLimitBenchQuery)
}

func BenchmarkResponseLimitOn(b *testing.B) {
	_, e := newFixtureExecutor(b, WithMaxResponseBytes(64<<20))
	benchRun(b, e, responseLimitBenchQuery)
}

// TestResponseLimitBenchQueryWritesData keeps the benchmark pair honest: a
// query that failed would time the error path and report both sides equal.
func TestResponseLimitBenchQueryWritesData(t *testing.T) {
	for name, opts := range map[string][]ExecutorOption{
		"off": nil,
		"on":  {WithMaxResponseBytes(64 << 20)},
	} {
		_, e := newFixtureExecutor(t, opts...)
		resp := run(t, e, responseLimitBenchQuery, "")
		if len(resp.Errors) != 0 || len(resp.Data) < 100 {
			t.Fatalf("%s: %d bytes, errors %s", name, len(resp.Data), errorsJSON(resp.Errors))
		}
		t.Logf("%s: %d bytes of data", name, len(resp.Data))
	}
}
