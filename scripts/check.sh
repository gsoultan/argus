#!/usr/bin/env bash
#
# Everything CI would run, runnable locally with one command.
#
# Runs every check even when an early one fails, then reports the full tally —
# fixing three problems in one pass beats three round-trips.
#
#   ./scripts/check.sh
#   ./scripts/check.sh web      only the web checks
#   ./scripts/check.sh go       only the Go checks
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

TARGET="${1:-all}"
FAILED=()

run_check() {
  local label=$1; shift
  printf '  %-22s' "$label"
  local out
  if out=$("$@" 2>&1); then
    printf '%s✓%s\n' "$GREEN" "$RESET"
  else
    printf '%s✗%s\n' "$RED" "$RESET"
    printf '%s\n' "$out" | sed 's/^/      /' | tail -25
    FAILED+=("$label")
  fi
}

banner "checks"

if [[ "$TARGET" == all || "$TARGET" == web ]]; then
  [[ -d "$WEB_DIR/node_modules" ]] || die "Web dependencies missing — run ./scripts/setup.sh"
  step "Web"
  run_check "typecheck" bash -c "cd '$WEB_DIR' && bun run typecheck"
  run_check "production build" bash -c "cd '$WEB_DIR' && bunx vite build --logLevel error"
fi

if [[ "$TARGET" == all || "$TARGET" == go ]]; then
  if has_backend; then
    step "Go"
    # Control-plane tests exercise real database behaviour — atomic ticket
    # redemption, the row lock on approvals, the advisory lock serialising the
    # audit chain. Without a database they skip rather than fail, so a missing
    # Postgres silently shrinks the suite.
    if [[ -z "${ARGUS_TEST_DATABASE_URL:-}" ]]; then
      export ARGUS_TEST_DATABASE_URL="postgres://argus:argus@localhost:5433/argus?sslmode=disable"
    fi
    # Probe the database this actually runs against, not a hardcoded one. The
    # storm check below gates on reachability rather than only warning, so a
    # DSN pointing anywhere else printed SKIPPED while looking like it ran.
    db_hostport=${ARGUS_TEST_DATABASE_URL#*://}
    db_hostport=${db_hostport#*@}
    db_hostport=${db_hostport%%/*}
    db_host=${db_hostport%%:*}
    db_port=${db_hostport#*:}
    [[ "$db_port" == "$db_host" ]] && db_port=5432
    if ! (exec 3<>"/dev/tcp/$db_host/$db_port") 2>/dev/null; then
      warn "Postgres is not reachable at $db_host:$db_port — control-plane tests will SKIP (run: ./scripts/deps.sh up)"
    fi
    # Not ./... — Go does not skip node_modules, so an npm dependency that
    # ships Go source (flatted does) joins the build. Compiling third-party
    # code that arrived through a JavaScript lockfile is not something this
    # product should do by accident.
    packages=$(go list ./... | grep -v '/node_modules/')
    run_check "vet" go vet $packages
    run_check "test" go test $packages
    # gofmt exits 0 even when files need formatting, so check for output instead.
    run_check "gofmt" bash -c '[[ -z "$(gofmt -l .)" ]] || { gofmt -l .; exit 1; }'
    # The storm model in internal/control/rmodel is a PROJECTION of this
    # schema; migrations/ is its source of truth and storm never applies DDL.
    # Nothing keeps the two aligned except this: `verify` fails when the
    # database has a column, index or constraint the model does not, or the
    # other way round. Without it the generated readers drift silently from
    # the tables they read, which is the one failure mode adopting a
    # code generator introduces.
    #
    # Two directions, because they fail differently. `verify` compares the
    # model to the database and needs one; `verify -stale` compares the
    # committed rgen packages to the model and needs nothing, so it runs
    # wherever this does. Without the second, 28k lines of generated reader can
    # drift from the model that is supposed to describe them.
    run_check "storm stale" go run ./cmd/stormgen verify -stale internal/control/rgen
    if (exec 3<>"/dev/tcp/$db_host/$db_port") 2>/dev/null; then
      run_check "storm verify" go run ./cmd/stormgen verify -dsn "$ARGUS_TEST_DATABASE_URL"
    else
      warn "Postgres is not reachable at $db_host:$db_port — storm verify SKIPPED"
    fi
  elif [[ "$TARGET" == go ]]; then
    die "No go.mod — there is no Go code to check yet."
  fi
fi

printf '\n'
if (( ${#FAILED[@]} )); then
  fail "${#FAILED[@]} check(s) failed: ${FAILED[*]}"
  exit 1
fi
ok "All checks passed"
