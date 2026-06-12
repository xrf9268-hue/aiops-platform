# issue #777 — protocol §4 poll-loop shell hardening

One earned rule into `docs/runbooks/pr-review-merge-protocol.md` §4 (single
source of truth; handle-issue/handle-pr/batch runbook reference it and get the
rule for free):

Observed failure (PR #774 second-round codex watch, 2026-06-12): rich-text
JSON (comment `body` with `\n` escapes) piped through `echo "$var"` under zsh
(builtin echo interprets backslash escapes by default) gained real control
characters → JSONDecodeError → swallowed by `2>/dev/null` → watcher spun
silently 16+ rounds.

Verified before writing: root cause A/B-reproduced (echo fails char 221,
printf parses clean, od shows the injected newline; man zshbuiltins cites the
escape behavior); repo scripts unaffected (bash shebangs, scalar variables);
§4's own commands already scalar-only `--jq`; corrected pattern field-tested
on the PR #776 watch (signal hit in 4 rounds, ERR-loud on failure).

Constraint text to add at the end of §4 item 2: predicates inside `--jq`
emitting scalars/flags only; `printf '%s'` when a variable must carry JSON;
never swallow producer stderr in a poll loop — print an `ERR:` line on
fetch/parse failure.
