// Command buildbench measures what a schema costs to generate and compile,
// for graphql-go's gqlc against gqlgen, at several schema sizes.
//
//	go run ./cmd/buildbench -n 10,50,200
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/syssam/graphql-go/benchmarks/buildbench"
)

func main() {
	sizes := flag.String("n", "10,50", "comma-separated entity counts")
	engines := flag.String("engines", "gqlc,gqlgen", "comma-separated engines")
	split := flag.Bool("split", false, "one SDL file per entity, so gqlc emits one package per entity")
	keep := flag.Bool("keep", false, "keep the temporary modules for inspection")
	flag.Parse()

	root, err := buildbench.RepoRoot(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var results []buildbench.Result
	for _, s := range strings.Split(*sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad -n value %q: %v\n", s, err)
			os.Exit(1)
		}
		for _, engine := range strings.Split(*engines, ",") {
			engine = strings.TrimSpace(engine)
			fmt.Fprintf(os.Stderr, "running %s at %d entities...\n", engine, n)
			r := buildbench.Run(engine, n, root, *split, *keep)
			results = append(results, r)
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "  FAILED: %v\n", r.Err)
				continue
			}
			fmt.Fprintf(os.Stderr, "  gen %s, build %s, %d LOC in %d pkgs\n",
				round(r.GenWall), round(r.ColdBuild), r.GenLOC, r.GenPkgs)
		}
	}

	fmt.Println()
	fmt.Println("| Entities | Layout | Engine | Generate | Peak RSS | Pkgs | LOC | Compile | Regen | Rebuild | Edit&rarr;built |")
	fmt.Println("|---:|---|---|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("| %d | %s | %s | FAILED: %s |\n",
				r.Entities, layout(r.Split), r.Engine, firstLine(r.Err.Error()))
			continue
		}
		fmt.Printf("| %d | %s | %s | %s | %s | %d | %s | %s | %s | %s | %s |\n",
			r.Entities, layout(r.Split), r.Engine,
			round(r.GenWall), mem(r.GenPeakKB), r.GenPkgs, thousands(r.GenLOC),
			round(r.ColdBuild), round(r.IncrGen), round(r.Incr),
			round(r.IncrGen+r.Incr))
	}
}

func round(d time.Duration) string {
	if d >= time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Millisecond).String()
}

func mem(kb uint64) string {
	if kb == 0 {
		return "n/a"
	}
	if kb >= 1<<20 {
		return fmt.Sprintf("%.2f GB", float64(kb)/(1<<20))
	}
	return fmt.Sprintf("%.0f MB", float64(kb)/1024)
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	return string(out)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func layout(split bool) string {
	if split {
		return "split"
	}
	return "flat"
}
