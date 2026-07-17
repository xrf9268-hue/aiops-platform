#!/usr/bin/env bash
set -euo pipefail

repo="${1:?usage: holdout.sh REPO_DIR}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
db="$tmp/todos.json"
bin="$tmp/todo"

(cd "$repo" && go test ./... && go vet ./... && go build -o "$bin" .)

expect_eq() {
  local actual="$1" expected="$2" label="$3"
  if [ "$actual" != "$expected" ]; then
    printf '%s: got %q; want %q\n' "$label" "$actual" "$expected" >&2
    exit 1
  fi
}

expect_eq "$(TODO_DB="$db" "$bin" add '  Buy milk  ')" "added 1" "trimmed add"
if TODO_DB="$db" "$bin" add '   ' >"$tmp/blank.out" 2>"$tmp/blank.err"; then
  printf 'blank title unexpectedly succeeded\n' >&2
  exit 1
fi
expect_eq "$(cat "$tmp/blank.err")" "title must not be empty" "blank title error"
expect_eq "$(TODO_DB="$db" "$bin" add 'Ship report')" "added 2" "monotonic id"
expect_eq "$(TODO_DB="$db" "$bin" list)" $'1\tactive\tBuy milk\n2\tactive\tShip report' "initial list"
expect_eq "$(TODO_DB="$db" "$bin" done 1)" "completed 1" "complete"
expect_eq "$(TODO_DB="$db" "$bin" done 1)" "completed 1" "idempotent complete"
expect_eq "$(TODO_DB="$db" "$bin" list --status active)" $'2\tactive\tShip report' "active filter"
expect_eq "$(TODO_DB="$db" "$bin" list --status done)" $'1\tdone\tBuy milk' "done filter"

before="$(shasum -a 256 "$db")"
if TODO_DB="$db" "$bin" done 999 >"$tmp/missing.out" 2>"$tmp/missing.err"; then
  printf 'missing id unexpectedly succeeded\n' >&2
  exit 1
fi
after="$(shasum -a 256 "$db")"
expect_eq "$after" "$before" "missing id database mutation"
if ! grep -q '999' "$tmp/missing.err"; then
  printf 'missing-id error does not name 999: %s\n' "$(cat "$tmp/missing.err")" >&2
  exit 1
fi

if TODO_DB="$db" "$bin" list --status invalid >"$tmp/status.out" 2>"$tmp/status.err"; then
  printf 'invalid status unexpectedly succeeded\n' >&2
  exit 1
fi
expect_eq "$(shasum -a 256 "$db")" "$before" "invalid status database mutation"

printf 'HOLDOUT PASS\n'
