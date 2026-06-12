# PRD: typed 429/Retry-After classification (#761)

See https://github.com/xrf9268-hue/aiops-platform/issues/761 — it is the
authoritative requirements doc. Scope guard: minimal hardening only
(harness principle 6) — typed classification + Retry-After surfacing;
NO client-side rate budgeting, NO poll scheduling changes, NO new config.
Also switch error strings to numeric status codes (resp.StatusCode).

Acceptance criteria: per issue #761 (typed errors.Is/As-classifiable 429
across all three clients carrying parsed Retry-After; tests x3 cases per
client; poll-loop skip-tick behavior unchanged + pinned by test; numeric
status codes in error strings).
