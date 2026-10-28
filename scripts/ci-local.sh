#!/usr/bin/env bash
# Run .github/workflows/ci.yml locally, in Docker, on Linux.
#
#   scripts/ci-local.sh                 every job CI runs on a push
#   scripts/ci-local.sh test postgres   only those jobs
#   scripts/ci-local.sh host            the test job natively, on a copy: this OS's leg
#   scripts/ci-local.sh flake           not in CI: every module's race suite ten times, shuffled
#   FUZZTIME=5s scripts/ci-local.sh fuzz
#
# This file and the workflow mirror each other: a step added to one belongs in
# the other. The versions are not written here a second time -- the Go image
# comes from go.mod, and the PostgreSQL image and the golangci-lint version are
# read out of the workflow -- so they cannot drift, and the script stops if it
# can no longer find them.
#
# What it is for is failing whenever CI would. So:
#
#   * The tree is copied into each container, not mounted. A bind mount keeps
#     the host filesystem's case rules, and the tests that only run where case
#     matters would skip on a Mac or a Windows machine exactly as they do
#     natively.
#   * Each job starts from a fresh copy, as each CI job starts from a fresh
#     checkout: the copy is the working tree, uncommitted changes included,
#     committed inside the container so `git diff` means what it means in CI.
#   * Tools are built inside the container by the toolchain go.mod names. A
#     golangci-lint or govulncheck built by another Go exits 0 having analysed
#     nothing.
#   * PostgreSQL is waited for over TCP. Over the socket, pg_isready answers
#     while the image's init server is still running, and the real one starts
#     under the tests.
#   * The PostgreSQL job proves it ran on PostgreSQL. With its variable unset
#     or misspelled the suite runs on SQLite and passes; here the server has
#     to have created the tests' databases, and no test may skip.
#   * Nothing of yours is touched: the server has no host port, its data is in
#     memory, and it and its network are removed on exit.
#
# The workflow's test job also runs on Windows and macOS. The host job runs
# its steps natively, on a fresh copy of the tree like the others, with this
# machine's Go: the macOS leg on a Mac, the Windows leg under Git Bash on
# Windows. It needs no Docker, and it is in the default set wherever the
# machine is not Linux, whose leg the test job already is.
#
# And one thing it covers that CI does not: the tree here is the working
# tree, so a file never `git add`ed is in it. Those are listed when there are
# any, since CI will not have them.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
workflow=.github/workflows/ci.yml

go_version=$(awk '$1 == "go" { print $2; exit }' go.mod)
pg_image=$(awk '$1 == "image:" && $2 ~ /^postgres:/ { print $2; exit }' "$workflow")
golangci=$(grep -o 'golangci-lint@v[0-9.]*' "$workflow" | head -1 | cut -d@ -f2 || true)
for v in go_version pg_image golangci; do
  if [ -z "${!v}" ]; then
    echo "ci-local: could not read $v; has $workflow or go.mod changed shape?" >&2
    exit 2
  fi
done
go_image="golang:$(echo "$go_version" | cut -d. -f1-2)"

# Docker for every job but host, which runs on this machine.
needs_docker=1
if [ "$#" -gt 0 ]; then
  needs_docker=0
  for job in "$@"; do [ "$job" = host ] || needs_docker=1; done
fi
if [ "$needs_docker" = 1 ]; then
  command -v docker >/dev/null || { echo "ci-local: docker is required" >&2; exit 2; }
fi

run_id="graphql-go-ci-$$"
work=$(mktemp -d "${TMPDIR:-/tmp}/graphql-go-ci.XXXXXX")
logs="$work/logs"
mkdir -p "$logs"
keep_logs=0
cleanup() {
  # The job's container too: killed mid-job, this script is gone and the
  # container is not, and a network with something attached cannot be removed.
  docker rm -f -v "$run_id-go" "$run_id-pg" >/dev/null 2>&1 || true
  docker network rm "$run_id" >/dev/null 2>&1 || true
  if [ "$keep_logs" = 1 ]; then
    rm -f "$work/src.tar"
    echo "logs: $logs"
  else
    rm -rf "$work"
  fi
}
trap cleanup EXIT
# A trapped signal waits for a foreground command to return, and the job in a
# container is minutes long; so each job runs in the background and is waited
# for, which a signal does interrupt. Then cleanup removes the container.
trap 'exit 130' INT TERM

# The working tree as git sees it: tracked and untracked files, nothing
# ignored, nothing deleted.
#
# COPYFILE_DISABLE stops macOS's tar writing a ._name beside every file that
# has extended attributes. Unpacked on Linux those are files, and the
# generated packages embed schema/*.graphql: the first run of this script
# failed every schema-building test on "._velox_order.graphql".
#
# Files and symlinks only. git lists an untracked directory that is itself a
# repository as one entry, and tar would take all of it, .git included.
git ls-files -co --exclude-standard -z \
  | while IFS= read -r -d '' f; do
      if [ -L "$f" ] || { [ -e "$f" ] && [ ! -d "$f" ]; }; then printf '%s\0' "$f"; fi
    done \
  | COPYFILE_DISABLE=1 tar --null -T - -cf "$work/src.tar"

untracked=$(git ls-files -o --exclude-standard)
if [ -n "$untracked" ]; then
  echo "in the tree under test and not in git, so not in what CI will see:"
  echo "$untracked" | sed 's/^/    /'
fi

# The images CI would get today, not whichever patch release was pulled last.
if [ "$needs_docker" = 1 ]; then
  for image in "$go_image" "$pg_image"; do
    docker pull -q "$image" >/dev/null 2>&1 || echo "ci-local: could not pull $image; using the local copy"
  done
  docker network create "$run_id" >/dev/null
fi


# Set only while the PostgreSQL job runs, and never taken from the caller's
# environment: a variable left set in a shell must not point CI's tests at
# somebody's database.
pg_url=""
pg_required=""

# Module and build caches survive between runs, as CI's do; the source never does.
#
# The ._ check is here, where the archive is unpacked: macOS's own tar hides
# those entries when it lists an archive, so a check on the host cannot see
# what it is looking for.
prelude='
mkdir -p /src && tar -xf /ci/src.tar -C /src && cd /src
if [ -n "$(find . -name "._*" -print -quit)" ]; then
  echo "ci-local: the copy of the tree holds ._ metadata files: $(find . -name "._*" | head -3)"; exit 3
fi
git init -q . && git -c user.name=ci -c user.email=ci@local add -A >/dev/null
git -c user.name=ci -c user.email=ci@local commit -qm "the tree under test"
'
in_go() { # in_go <log> <script>: run script in a fresh copy of the tree
  docker run --rm --init --name "$run_id-go" --network "$run_id" \
    -e GOTOOLCHAIN=local -e FUZZTIME="${FUZZTIME:-30s}" -e BACKOFFICE_POSTGRES="$pg_url" \
    -e BACKOFFICE_REQUIRE_POSTGRES="$pg_required" \
    -v "$work/src.tar:/ci/src.tar:ro" \
    -v graphql-go-ci-go:/go -v graphql-go-ci-build:/root/.cache/go-build \
    "$go_image" bash -euo pipefail -c "$prelude$2" >"$1" 2>&1 &
  wait $!
}

gen_compare='(cd compare && go run gen.go -n 25)'

test_steps='
unformatted=$(gofmt -l $(git ls-files "*.go"))
if [ -n "$unformatted" ]; then echo "not formatted:"; echo "$unformatted"; exit 1; fi
'"$gen_compare"'
GATE_REQUIRE_ALL=1 sh scripts/gate.sh
(cd examples/blog && go generate ./...)
(cd examples/backoffice && go generate .)
changed=$(git status --porcelain -- .)
if [ -n "$changed" ]; then
  echo "generated code is stale or not committed; run go generate in examples/blog or examples/backoffice"
  echo "$changed"; git diff --stat; exit 1
fi
# Not a CI step: say how much of the suite this platform left out.
echo "skipped here: $(go test -count=1 -v ./codegen 2>/dev/null | grep -c "^--- SKIP" || true) codegen tests"
'

job_test() {
  in_go "$1" "$test_steps"
}

# The test job's steps on this machine rather than in a container: a fresh
# copy of the tree, committed so that git diff means what it means in CI, and
# the Go on PATH, which go.mod's toolchain line must already match.
job_host() {
  local copy="$work/host"
  mkdir -p "$copy" && tar -xf "$work/src.tar" -C "$copy"
  (
    cd "$copy"
    git init -q . && git -c user.name=ci -c user.email=ci@local add -A >/dev/null
    git -c user.name=ci -c user.email=ci@local commit -qm "the tree under test"
    # The suite here is the test job's, which has no database. A URL left in
    # the caller's environment would point the backoffice tests at that
    # server, where they drop and create databases.
    env -u BACKOFFICE_POSTGRES -u BACKOFFICE_REQUIRE_POSTGRES \
      GOTOOLCHAIN=local bash -euo pipefail -c "$test_steps"
  ) >"$1" 2>&1
}

job_postgres() {
  docker run -d --name "$run_id-pg" --network "$run_id" --network-alias ci-postgres \
    -e POSTGRES_PASSWORD=pass --tmpfs /var/lib/postgresql/data \
    "$pg_image" -c log_statement=ddl >/dev/null
  local tries=0
  until docker exec "$run_id-pg" pg_isready -q -h 127.0.0.1 -U postgres -d postgres; do
    tries=$((tries + 1))
    if [ "$tries" -gt 60 ]; then echo "PostgreSQL did not come up" >"$1"; return 1; fi
    sleep 1
  done
  local status=0
  pg_url="postgres://postgres:pass@ci-postgres:5432/postgres?sslmode=disable"
  pg_required=1
  in_go "$1" 'go -C examples/backoffice test -race -count=1 -v ./...' || status=$?
  pg_url=""
  pg_required=""
  local passed skipped created
  # Subtests too: theirs are indented.
  passed=$(grep -c '^ *--- PASS' "$1" || true)
  skipped=$(grep -c '^ *--- SKIP' "$1" || true)
  created=$(docker logs "$run_id-pg" 2>&1 | grep -c 'CREATE DATABASE backoffice_' || true)
  docker rm -f -v "$run_id-pg" >/dev/null 2>&1 || true
  detail="$passed tests passed, $skipped skipped, $created databases created on $pg_image"
  if [ "$status" != 0 ]; then return "$status"; fi
  if [ "$passed" = 0 ] || [ "$skipped" != 0 ] || [ "$created" = 0 ]; then
    echo "ci-local: the suite passed without proving it ran on PostgreSQL: $detail" >>"$1"
    return 1
  fi
}

job_lint() {
  in_go "$1" '
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@'"$golangci"'
golangci-lint run ./...
'"$gen_compare"'
mods=$(find . -name go.mod -not -path "./ref/*" | sed "s|/go.mod$||")
[ -n "$mods" ]
for mod in $mods; do
  echo "== tidy $mod"
  (cd "$mod" && go mod tidy -diff)
done
for mod in $(find . -name go.mod -not -path "./ref/*" -not -path "./go.mod" | sed "s|/go.mod$||"); do
  echo "== $mod"
  (cd "$mod" && golangci-lint run ./...)
done
(cd lint && go build -o /tmp/gqlvet ./cmd/gqlvet)
status=0
/tmp/gqlvet ./scripts/testdata/gqlvet || status=$?
if [ "$status" != 3 ]; then
  echo "gqlvet exited $status on scripts/testdata/gqlvet, which it must report (exit 3)"
  exit 1
fi
mods=$(find . -name go.mod -not -path "./ref/*" | sed "s|/go.mod$||")
[ -n "$mods" ]
for mod in $mods; do
  echo "== $mod"
  (cd "$mod" && /tmp/gqlvet ./...)
done
'
}

job_bench() {
  # The timings CI records beside this are never asserted, so they are not run.
  in_go "$1" '
go test -count=1 -run "^TestAllocationBaseline$" -v . | tee /tmp/alloc.txt
if ! grep -q "^--- PASS: TestAllocationBaseline " /tmp/alloc.txt; then
  echo "TestAllocationBaseline did not run and pass"
  exit 1
fi
'
}

job_vuln() {
  in_go "$1" '
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck -version
'"$gen_compare"'
for mod in $(find . -name go.mod -not -path "./ref/*" | sed "s|/go.mod$||"); do
  echo "== $mod"
  (cd "$mod" && govulncheck ./...)
done
'
}

job_fuzz() {
  in_go "$1" '
pkgs=$(go list -f "{{.ImportPath}} {{.Dir}}" ./...)
ran=0
while read -r pkg dir <&3; do
  listed=$(go test -list "Fuzz.*" "$pkg")
  targets=$(printf "%s\n" "$listed" | grep "^Fuzz" || true)
  if [ -z "$targets" ] && grep -qs "^func Fuzz" "$dir"/*_test.go; then
    echo "$pkg declares a fuzz target and go test -list found none"
    exit 1
  fi
  for target in $targets; do
    echo "== $pkg $target"
    go test -run "^$" -fuzz "^${target}$" -fuzztime "$FUZZTIME" "$pkg"
    ran=$((ran + 1))
  done
done 3<<<"$pkgs"
if [ "$ran" = 0 ]; then
  echo "no fuzz target was found in any package"
  exit 1
fi
'
}

# Not a CI job. A race that fails a few runs in a hundred passes once here and
# fails there; run this before pushing a change to anything concurrent. Every
# module, as the gate does, FLAKE_COUNT times (10) each in a shuffled order.
job_flake() {
  in_go "$1" "$gen_compare"'
for m in $(find . -name go.mod -not -path "./ref/*" | sed "s|/go.mod$||" | sort); do
  echo "== $m"
  (cd "$m" && go test -race -count='"${FLAKE_COUNT:-10}"' -shuffle=on -short ./...)
done
'
}

jobs=("$@")
if [ "${#jobs[@]}" -eq 0 ]; then
  jobs=(test postgres lint bench vuln fuzz)
  # Linux's leg is the test job; anywhere else this machine has a leg of its own.
  [ "$(uname -s)" = Linux ] || jobs+=(host)
fi

# Before any job runs: a misspelled name found after the first job would end
# the script and take that job's logs with it.
for job in "${jobs[@]}"; do
  if ! declare -F "job_$job" >/dev/null; then
    echo "ci-local: no job called $job (test postgres lint bench vuln fuzz host flake)" >&2
    exit 2
  fi
done

echo "go $go_version ($go_image), $pg_image, golangci-lint $golangci"
failed=()
for job in "${jobs[@]}"; do
  detail=""
  start=$(date +%s)
  printf '%-9s ' "$job"
  if "job_$job" "$logs/$job.log"; then
    result=PASS
  else
    result=FAIL
    failed+=("$job")
  fi
  took=$(( $(date +%s) - start ))
  echo "$result  ${took}s${detail:+  $detail}"
  case "$job" in
    test | host) grep -E '^(root|benchmarks|compare|examples/|lint|skipped here)' "$logs/$job.log" | sed 's/^/            /' || true ;;
    fuzz) echo "            $(grep -c '^== ' "$logs/$job.log" || true) targets, ${FUZZTIME:-30s} each" ;;
  esac
  if [ "$result" = FAIL ]; then
    echo "---- last lines of $job"
    tail -n 30 "$logs/$job.log" | sed 's/^/    /'
  fi
done

if [ "${#failed[@]}" -gt 0 ]; then
  keep_logs=1
  echo "FAILED: ${failed[*]}"
  exit 1
fi
echo "every job passed"
