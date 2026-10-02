#!/bin/sh
# measure.sh <layout-dir> <gocache> : cold build, incremental rebuilds, binary
# size and start-up cost of one generated layout. Prints one line per figure.
set -eu
D=$1
export GOCACHE=$2
cd "$D"

# timed <label> : build, report wall / cpu / peak RSS / packages compiled.
timed() {
	/usr/bin/time -l go build -v -o server ./cmd/server 2> .time.txt || { cat .time.txt; exit 1; }
	real=$(awk '/ real /{print $1}' .time.txt)
	cpu=$(awk '/ real /{printf "%.2f", $3+$5}' .time.txt)
	rss=$(awk '/maximum resident set size/{printf "%.0f", $1/1048576}' .time.txt)
	pk=$(grep -c '^scale' .time.txt || true)
	printf '%-34s wall=%6.2fs cpu=%7.2fs peak_rss=%5sMB packages=%s\n' "$1" "$real" "$cpu" "$rss" "$pk"
}

timed "cold build"
printf '%-34s %s MB\n' "binary" "$(ls -l server | awk '{printf "%.1f", $5/1048576}')"

for rep in 1 2 3; do
	sed -i '' "s/qty must not be negative[!]*/qty must not be negative$(printf '!%.0s' $(seq 1 $rep))/" internal/service/e150/e150.go
	timed "service body edit #$rep"
done
for rep in 1 2 3; do
	printf '\nfunc (s *Service) Extra%d() int { return %d }\n' "$rep" "$rep" >> internal/service/e150/e150.go
	timed "service API edit #$rep"
done
for rep in 1 2 3; do
	printf '\nfunc (r *Handler) extra%d() int { return %d }\n' "$rep" "$rep" >> graph/e150/e150.resolvers.go
	timed "handler edit #$rep"
done
if grep -q "^type Services struct" internal/service/service.go; then
	for rep in 1 2 3; do
		sed -i '' "s/^\tE000 \*e000.Service$/\tE000 *e000.Service\n\tAdded$rep *e000.Service/" internal/service/service.go
		timed "new field on Services #$rep"
	done
fi

for i in 1 2 3 4 5 6 7 8 9 10 11; do ./server; done | sort -t= -k2 -n | sed -n 6p | sed 's/^/start-up (median of 11 by time)     /'
