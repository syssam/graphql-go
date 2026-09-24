// Command memlimit runs the engine under a cgroup memory limit, so what a
// container does to this server is measured rather than assumed.
//
//	docker run --rm --memory=256m -v "$PWD:/src" -w /src/benchmarks golang:1.27 \
//	  sh -c 'go run ./cmd/memlimit -users 20000 -c 8'
//
// It prints what the runtime sees, then executes a query whose response is
// large enough to matter, and reports peak heap. Run it twice, once with
// GOMEMLIMIT set and once without: the difference is the point.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/syssam/graphql-go"
)

const sdl = `
type Row { id: ID! a: String! b: String! c: String! }
type Query { rows: [Row!]! }
`

type row struct{ ID, A, B, C string }

func dataset(n int) []*row {
	rows := make([]*row, n)
	for i := range rows {
		id := strconv.Itoa(i)
		rows[i] = &row{
			ID: id,
			A:  "field a value for row " + id,
			B:  "field b value for row " + id,
			C:  "field c value for row " + id,
		}
	}
	return rows
}

func newExecutor(rows []*row) *graphql.Executor {
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Object[row]("Row",
			graphql.Field("id", func(r *row) graphql.ID { return graphql.ID(r.ID) }),
			graphql.Field("a", func(r *row) string { return r.A }),
			graphql.Field("b", func(r *row) string { return r.B }),
			graphql.Field("c", func(r *row) string { return r.C }),
		),
		graphql.Query(
			graphql.Resolve("rows", func(_ context.Context, _ graphql.Root) ([]*row, error) {
				return rows, nil
			}),
		),
	)
	if err != nil {
		panic(err)
	}
	return graphql.NewExecutor(s)
}

// cgroupLimit reports the container's memory limit, or 0 outside one. Only
// cgroup v2 is read; that is what every current runtime gives a container.
func cgroupLimit() int64 {
	b, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0
	}
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0 // "max": no limit
	}
	return n
}

func mib(n uint64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

func main() {
	users := flag.Int("users", 20000, "rows in the response")
	conc := flag.Int("c", 8, "concurrent requests")
	rounds := flag.Int("rounds", 40, "rounds of -c requests")
	flag.Parse()

	limit := cgroupLimit()
	memLimit := debug.SetMemoryLimit(-1)
	fmt.Printf("GOMAXPROCS=%d NumCPU=%d\n", runtime.GOMAXPROCS(0), runtime.NumCPU())
	if limit > 0 {
		fmt.Printf("cgroup memory.max = %s\n", mib(uint64(limit)))
	} else {
		fmt.Println("cgroup memory.max = (none)")
	}
	if memLimit == 1<<63-1 {
		fmt.Println("GOMEMLIMIT        = unset -- the runtime will not hold back for the cgroup")
	} else {
		fmt.Printf("GOMEMLIMIT        = %s\n", mib(uint64(memLimit)))
	}

	rows := dataset(*users)
	e := newExecutor(rows)
	req := &graphql.Request{Query: `{ rows { id a b c } }`}

	// One response first, to report its size: the whole point is how big a
	// single response is against the container it runs in.
	resp := e.Execute(context.Background(), req)
	if len(resp.Errors) > 0 {
		fmt.Println("query failed:", resp.Errors[0])
		os.Exit(1)
	}
	respSize := len(resp.Data)
	resp.Release()
	fmt.Printf("one response      = %s\n\n", mib(uint64(respSize)))

	var peak uint64
	var done atomic.Int64
	stop := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak {
				peak = ms.HeapAlloc
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	start := time.Now()
	for r := range *rounds {
		var wg sync.WaitGroup
		for range *conc {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp := e.Execute(context.Background(), req)
				if len(resp.Errors) == 0 {
					done.Add(1)
				}
				resp.Release()
			}()
		}
		wg.Wait()
		if r == *rounds/2 {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			fmt.Printf("halfway: %d responses, heap %s, sys %s\n",
				done.Load(), mib(ms.HeapAlloc), mib(ms.Sys))
		}
	}
	close(stop)
	watcher.Wait()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("\nsurvived %d responses in %v\n", done.Load(), time.Since(start).Round(time.Millisecond))
	fmt.Printf("peak heap %s, final heap %s, sys %s, GCs %d\n",
		mib(peak), mib(ms.HeapAlloc), mib(ms.Sys), ms.NumGC)
}
