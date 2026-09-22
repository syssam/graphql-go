package graphql

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateAllocs = flag.Bool("update-allocs", false, "rewrite docs/alloc-baseline.txt from a fresh run")

// allocBaseline is the file this test enforces: one benchmark per line,
// "Name allocs". Nothing else is recorded, and that is the point.
const allocBaseline = "docs/alloc-baseline.txt"

// Benchmarks whose allocation count is a contract. The functions are named
// directly rather than looked up by string, so renaming or deleting one is a
// compile error here instead of a gate that quietly stops guarding anything.
var allocGated = []struct {
	name string
	fn   func(*testing.B)
}{
	{"BenchmarkExecuteUsers", BenchmarkExecuteUsers},
	{"BenchmarkFieldPathBare", BenchmarkFieldPathBare},
	{"BenchmarkExecuteTypenameHeavy", BenchmarkExecuteTypenameHeavy},
	{"BenchmarkExecuteNoAuthorizer", BenchmarkExecuteNoAuthorizer},
	{"BenchmarkExecuteWithAuthorizer", BenchmarkExecuteWithAuthorizer},
	{"BenchmarkListResultsSlice", BenchmarkListResultsSlice},
	{"BenchmarkQueryCostDisabled", BenchmarkQueryCostDisabled},
	{"BenchmarkResponseLimitOn", BenchmarkResponseLimitOn},
}

// Allocation counts are deterministic; timings on a shared CI runner are not.
// docs/performance.md says so in as many words, and this repository has been
// wrong by 20-77% reading a single timing sample on a warm machine.
//
// So this is the regression gate that survives a noisy runner: run the
// benchmarks that matter once each and require that none of them allocates
// more than the recorded baseline. Fewer is fine and is reported, because a
// win nobody notices tends to be given back.
//
//	go test -run TestAllocationBaseline -update-allocs .
//
// Deliberately one-sided. Pinning the exact number would make every
// improvement a failing test, and the failure people learn to update without
// reading is worth less than no test at all.
func TestAllocationBaseline(t *testing.T) {
	if raceEnabled {
		t.Skip("-race changes allocation counts; the baseline is for an ordinary build")
	}
	if testing.Short() {
		t.Skip("runs benchmarks")
	}

	got := make(map[string]int64, len(allocGated))
	for _, b := range allocGated {
		r := testing.Benchmark(b.fn)
		if r.N == 0 {
			t.Fatalf("%s did not run", b.name)
		}
		got[b.name] = r.AllocsPerOp()
	}

	if *updateAllocs {
		var b strings.Builder
		b.WriteString("# Allocations per operation, the figure that is deterministic.\n")
		b.WriteString("# Enforced one-sided by TestAllocationBaseline: more fails, fewer does not.\n")
		b.WriteString("# Regenerate: go test -run TestAllocationBaseline -update-allocs .\n")
		for _, g := range allocGated {
			fmt.Fprintf(&b, "%s %d\n", g.name, got[g.name])
		}
		if err := os.WriteFile(allocBaseline, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", allocBaseline)
		return
	}

	want, err := readAllocBaseline()
	if err != nil {
		t.Fatalf("%v\n\nRun: go test -run TestAllocationBaseline -update-allocs .", err)
	}
	for _, g := range allocGated {
		base, ok := want[g.name]
		if !ok {
			t.Errorf("%s has no baseline; regenerate with -update-allocs", g.name)
			continue
		}
		switch n := got[g.name]; {
		case n > base:
			t.Errorf("%s allocates %d/op, baseline %d: a regression of %d", g.name, n, base, n-base)
		case n < base:
			t.Logf("%s allocates %d/op, baseline %d -- an improvement worth recording with -update-allocs", g.name, n, base)
		}
	}
}

func readAllocBaseline() (map[string]int64, error) {
	f, err := os.Open(allocBaseline)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, num, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("%s: cannot parse %q", allocBaseline, line)
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("%s: %q: %w", allocBaseline, line, perr)
		}
		out[name] = n
	}
	return out, sc.Err()
}

// A duplicate in the list would run one benchmark twice and record the second
// answer, which is a gate that looks larger than it is.
func TestAllocGatedHasNoDuplicate(t *testing.T) {
	names := make([]string, len(allocGated))
	for i, g := range allocGated {
		names[i] = g.name
	}
	slices.Sort(names)
	if len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Fatal("allocGated contains a duplicate")
	}
}
