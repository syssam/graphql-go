// Command latency reports per-request latency percentiles for the engine.
//
//	go run ./cmd/latency -n 200000 -c 1
//	go run ./cmd/latency -n 200000 -c 8 -json
//
// Percentiles were withheld from docs/performance.md because the development
// machine's clock could not resolve them, so this measures the clock first and
// prints what it found. A percentile is only reported when the tick is small
// enough to carry it; otherwise the row says so instead of printing a number
// that is really the timer.
//
// It measures Executor.Execute in process, not over a socket: the transport
// suite (k6/README.md) already found that a loopback round trip is larger than
// anything it separates, so an HTTP percentile here would be a percentile of
// the socket.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/benchmarks/internal/data"
)

const sdl = `
type User { id: ID! name: String! email: String! friends: [User!]! }
type Query { users: [User!]! }
`

func newExecutor(users []*data.User) *graphql.Executor {
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Object[data.User]("User",
			graphql.Field("id", func(u *data.User) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *data.User) string { return u.Name }),
			graphql.Field("email", func(u *data.User) string { return u.Email }),
			graphql.Field("friends", func(u *data.User) []*data.User { return u.Friends }),
		),
		graphql.Query(
			graphql.Resolve("users", func(_ context.Context, _ graphql.Root) ([]*data.User, error) {
				return users, nil
			}),
		),
	)
	if err != nil {
		panic(err)
	}
	return graphql.NewExecutor(s)
}

// clockTick is the smallest non-zero difference two time.Now calls report. On
// Linux this is nanoseconds; on Windows it is the ~0.5 ms system tick, which is
// what made percentiles meaningless there.
func clockTick() time.Duration {
	best := time.Duration(1<<63 - 1)
	for range 200000 {
		a := time.Now()
		var d time.Duration
		for d == 0 {
			d = time.Since(a)
		}
		if d < best {
			best = d
		}
	}
	return best
}

type report struct {
	Name        string  `json:"name"`
	Samples     int     `json:"samples"`
	Concurrency int     `json:"concurrency"`
	ClockTickNs int64   `json:"clock_tick_ns"`
	MinNs       int64   `json:"min_ns"`
	P50Ns       int64   `json:"p50_ns"`
	P90Ns       int64   `json:"p90_ns"`
	P99Ns       int64   `json:"p99_ns"`
	P999Ns      int64   `json:"p999_ns"`
	MaxNs       int64   `json:"max_ns"`
	MeanNs      int64   `json:"mean_ns"`
	TicksPerP50 float64 `json:"ticks_per_p50"`
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func measure(name string, e *graphql.Executor, query string, n, conc int, tick time.Duration) report {
	req := &graphql.Request{Query: query}

	// Warm the plan cache and the writer pool: the first execution of a query
	// text compiles a plan, which is not what a steady-state percentile is
	// about and would otherwise land in the tail.
	for range 1000 {
		resp := e.Execute(context.Background(), req)
		if len(resp.Errors) > 0 {
			panic(resp.Errors[0])
		}
		resp.Release()
	}

	per := n / conc
	all := make([][]time.Duration, conc)
	var wg sync.WaitGroup
	for w := range conc {
		// Preallocated so the measurement loop allocates nothing of its own.
		all[w] = make([]time.Duration, per)
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := all[w]
			for i := range buf {
				start := time.Now()
				resp := e.Execute(context.Background(), req)
				buf[i] = time.Since(start)
				resp.Release()
			}
		}(w)
	}
	wg.Wait()

	samples := make([]time.Duration, 0, per*conc)
	for _, b := range all {
		samples = append(samples, b...)
	}
	slices.Sort(samples)

	var total time.Duration
	for _, d := range samples {
		total += d
	}
	p50 := percentile(samples, 0.50)
	return report{
		Name:        name,
		Samples:     len(samples),
		Concurrency: conc,
		ClockTickNs: tick.Nanoseconds(),
		MinNs:       samples[0].Nanoseconds(),
		P50Ns:       p50.Nanoseconds(),
		P90Ns:       percentile(samples, 0.90).Nanoseconds(),
		P99Ns:       percentile(samples, 0.99).Nanoseconds(),
		P999Ns:      percentile(samples, 0.999).Nanoseconds(),
		MaxNs:       samples[len(samples)-1].Nanoseconds(),
		MeanNs:      (total / time.Duration(len(samples))).Nanoseconds(),
		TicksPerP50: float64(p50) / float64(tick),
	}
}

func main() {
	n := flag.Int("n", 200000, "samples per query shape")
	conc := flag.Int("c", 1, "concurrent callers")
	asJSON := flag.Bool("json", false, "emit JSON")
	flag.Parse()

	users := data.Dataset()
	e := newExecutor(users)
	tick := clockTick()

	shapes := []struct{ name, query string }{
		{"tiny", `{ users { id } }`},
		{"shallow", `{ users { id name email } }`},
		{"nested", `{ users { id friends { id name } } }`},
	}
	reports := make([]report, 0, len(shapes))
	for _, s := range shapes {
		reports = append(reports, measure(s.name, e, s.query, *n, *conc, tick))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		_ = enc.Encode(reports)
		return
	}

	fmt.Printf("%s/%s  GOMAXPROCS=%d  concurrency=%d  samples=%d/shape\n",
		runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0), *conc, *n)
	fmt.Printf("clock tick: %v\n\n", tick)
	fmt.Printf("%-9s %10s %10s %10s %10s %10s %10s %12s\n",
		"shape", "min", "p50", "p90", "p99", "p99.9", "max", "ticks/p50")
	for _, r := range reports {
		fmt.Printf("%-9s %10v %10v %10v %10v %10v %10v %12.0f\n",
			r.Name,
			time.Duration(r.MinNs), time.Duration(r.P50Ns), time.Duration(r.P90Ns),
			time.Duration(r.P99Ns), time.Duration(r.P999Ns), time.Duration(r.MaxNs),
			r.TicksPerP50)
	}
	fmt.Println()
	// A percentile is only as fine as the clock under it. Stated as a ratio so
	// the reader can judge it rather than trust the tool.
	worst := reports[0]
	for _, r := range reports {
		if r.TicksPerP50 < worst.TicksPerP50 {
			worst = r
		}
	}
	if worst.TicksPerP50 < 10 {
		fmt.Printf("WARNING: the clock resolves p50 of %q to only %.1f ticks; these percentiles are the timer, not the engine.\n",
			worst.Name, worst.TicksPerP50)
	} else {
		fmt.Printf("The clock resolves the smallest p50 (%q) to %.0f ticks, so the percentiles are the engine.\n",
			worst.Name, worst.TicksPerP50)
	}
}
