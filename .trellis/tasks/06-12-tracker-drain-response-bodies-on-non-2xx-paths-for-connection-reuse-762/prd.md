# PRD: drain non-2xx response bodies (#762)

See https://github.com/xrf9268-hue/aiops-platform/issues/762 (authoritative).
Go's HTTP/1.1 transport only reuses a connection when the body is read to
EOF; closing unread tears it down, multiplying TCP/TLS churn exactly during
failure bursts. Fix: bounded drain (io.Copy to Discard via LimitReader)
before Close on all non-2xx paths in the Linear/GitHub/Gitea tracker
clients, via ONE shared helper (rule 3).

Interaction with #761 (now in main): githubSecondaryLimitBody already
consumes up to 8KB of some 403 bodies — the drain must still run for the
remainder; design the helper so both compose.

Acceptance: per issue #762 — all non-2xx paths drain bounded before close
via one shared helper; a connection-reuse test (httptest + connection
tracking) proves reuse across an error response; no behavior change to
error classification/messages.
