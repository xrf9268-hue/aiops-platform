# `isLoopbackHTTPHost` IPv6 support — design

**Date:** 2026-05-22
**Issue:** [#235](https://github.com/xrf9268-hue/aiops-platform/issues/235)

## Problem

`cmd/worker/main.go:423-435` validates the HTTP `Host` header against a literal allow-list of `"127.0.0.1"` and `"localhost"`. IPv6 loopback (`[::1]`) is rejected with **403 Forbidden**, even though it is the IPv6 host-equivalent of `127.0.0.1` per SPEC §13.7. The same rejection applies to `127.0.0.0/8` addresses other than `127.0.0.1` (e.g. `127.1.2.3`).

## Decision

Replace the literal allow-list with `net.ParseIP(host).IsLoopback()`. Keep the `"localhost"` special case because `net.ParseIP("localhost")` returns `nil` (it parses IPs, not names). Strip IPv6 brackets when the host arrived in bracket-without-port form (`"[::1]"`). Reject the `"::1"` (unbracketed) form — RFC 7230 requires IPv6 hosts in HTTP `Host` headers to be bracketed; an unbracketed `::1` is malformed.

The `net.IP.IsLoopback()` primitive accepts `127.0.0.0/8` and `::1`, and rejects routable addresses, so the rule becomes complete with minimal logic.

## What changes

| File | Change |
| --- | --- |
| `cmd/worker/main.go:423-435` | Rewrite `isLoopbackHTTPHost` to use `net.ParseIP(host).IsLoopback()` plus the `"localhost"` special case. Preserve the existing malformed-input rejection for `"host:port"` strings without IPv6 brackets that fail `SplitHostPort`. |
| `cmd/worker/main_test.go` | Add `TestIsLoopbackHTTPHost` table-driven cases covering IPv4, IPv4 loopback `127.0.0.0/8`, `localhost`, bracketed/unbracketed IPv6, non-loopback IP, and empty. Add `TestStateHTTPServerAllowsIPv6LoopbackHost` mirroring the existing IPv4 one. |

## New implementation

```go
func isLoopbackHTTPHost(hostport string) bool {
    if hostport == "" {
        return false
    }
    host := hostport
    if h, _, err := net.SplitHostPort(hostport); err == nil {
        host = h
    } else if strings.Contains(hostport, ":") && !strings.HasPrefix(hostport, "[") {
        // "host:port" without IPv6 brackets that failed to split — malformed.
        return false
    }
    // Trim IPv6 brackets when present without a port (e.g. "[::1]").
    host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
    if host == "localhost" {
        return true
    }
    if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
        return true
    }
    return false
}
```

## Test matrix

| Input | Expected |
| --- | --- |
| `"127.0.0.1:4000"` | true (IPv4 loopback + port) |
| `"127.0.0.1"` | true (IPv4 loopback, no port) |
| `"127.1.2.3:8080"` | true (other 127.0.0.0/8) |
| `"localhost:4000"` | true (named, with port) |
| `"localhost"` | true (named, no port) |
| `"[::1]:4000"` | true (IPv6 loopback, bracketed + port) |
| `"[::1]"` | true (IPv6 loopback, bracketed, no port) |
| `"::1"` | false (IPv6 without brackets — malformed per RFC 7230) |
| `"1.2.3.4:4000"` | false (routable IPv4) |
| `"evil.example"` | false (non-loopback name) |
| `""` | false |

## Non-goals

- Don't change the HTTP listener bind address — that's `cmd/worker/main.go` listener construction logic, not this validator.
- Don't accept routable IPv6 addresses just because they're IPv6 — security boundary stays at "loopback" per SPEC §13.7.
- Don't widen the special-case name list (e.g. `ip6-localhost`) — those don't appear in real HTTP `Host` headers, and adding them muddies the boundary.

## Acceptance criteria

- [ ] `[::1]` and `[::1]:<port>` Host headers pass.
- [ ] `127.0.0.0/8` IPv4 loopback addresses pass.
- [ ] Non-loopback IPs still rejected.
- [ ] Table-driven test and IPv6 handler test added.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/235
- Code: `cmd/worker/main.go:423-435`, existing tests at `main_test.go:595-631`
- SPEC §13.7 loopback default
- Stdlib: `net.IP.IsLoopback`
