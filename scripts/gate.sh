#!/usr/bin/env sh
# Run the standard gate across every module in the repository.
#
# `go vet ./... && go test -race ./...` reaches only the module it is run in,
# and this repository has five. benchmarks, compare and lint have been covered
# by whoever remembered to check them, which during one audit meant almost not
# at all. This walks every go.mod so that stops being a matter of memory.
#
#   scripts/gate.sh          vet and test every module
#   scripts/gate.sh -short   skip the slow subprocess and load tests
#
# compare/ needs its fixtures generated before its tests can run:
#
#   cd compare && go run gen.go -n 200
#
# Without them the script reports compare as skipped rather than failing, since
# a missing fixture is not a broken build. Set GATE_REQUIRE_ALL=1 to turn that
# skip into a failure: CI generates the fixtures, and a skip there means the
# gate is quietly covering one module less than it prints.
set -eu

short=""
[ "${1:-}" = "-short" ] && short="-short"

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

# ref/ holds vendored reference copies of other projects; they are not ours.
modules=$(find . -name go.mod -not -path "./ref/*" -not -path "*/.claude/*" \
  | sed 's|/go.mod$||' | sed 's|^\./||' | sort)

failed=""
skipped=""

for m in $modules; do
  name=$m
  [ "$m" = "." ] && name="root"

  if [ "$m" = "compare" ] && [ ! -d "compare/schema" ]; then
    skipped="$skipped $name"
    printf '%-12s skipped (run: cd compare && go run gen.go -n 200)\n' "$name"
    continue
  fi

  printf '%-12s ' "$name"
  if ! (cd "$m" && go vet ./... >/tmp/gate.$$ 2>&1); then
    echo "VET FAILED"
    cat /tmp/gate.$$
    failed="$failed $name"
    continue
  fi
  # Separate modules carry other projects' code and are slow; -race is kept
  # because the bug class it finds here is the whole reason it is mandatory.
  if ! (cd "$m" && go test -race -count=1 $short ./... >/tmp/gate.$$ 2>&1); then
    echo "TEST FAILED"
    grep -E "^(---|FAIL|ok)" /tmp/gate.$$ | grep -v "^ok" | head -20
    failed="$failed $name"
    continue
  fi
  echo "ok"
done
rm -f /tmp/gate.$$

if [ -n "$skipped" ]; then
  echo "skipped:$skipped"
  if [ "${GATE_REQUIRE_ALL:-}" = "1" ]; then
    echo "GATE_REQUIRE_ALL=1 and a module was skipped"
    exit 1
  fi
fi
if [ -n "$failed" ]; then
  echo "FAILED:$failed"
  exit 1
fi
echo "all modules pass"
