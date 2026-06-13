package orchestrator

import (
	"log"
	"runtime/debug"
)

// recoverPanic is the orchestrator's process-survival guard for code
// running on goroutines that the actor's serialization invariants do not
// otherwise protect: per-op `followup` closures, `time.AfterFunc` retry
// timers, and the spawn-result fan-out. SPEC §7.4 mandates one
// serialization authority for state mutation, so any panic that escaped
// these goroutines previously crashed the whole worker process — taking
// every unrelated in-flight run with it — rather than failing just the
// path that misbehaved.
//
// Usage:
//
//	defer recoverPanic("orchestrator.retry_timer")
//
// or wrap a goroutine launch via [safeGo].
//
// The handler logs a SPEC §13.1-shaped structured line (`event=panic
// site=<name>`) so operators tailing stderr / journald see a typed entry
// rather than the default `runtime/panic` trailer. The stack is captured
// inline so the log carries enough detail to diagnose the failure
// without a coredump.
func recoverPanic(site string) {
	if r := recover(); r != nil {
		recoverPanicValue(site, r)
	}
}

// recoverPanicValue is the same handler shape as recoverPanic but used
// from inside a `defer func() { if r := recover(); r != nil { … } }()`
// when the caller wants to consult the recovered value (for example,
// to clear a return variable). Centralizing the log shape here keeps
// every panic site emitting the same structured line.
func recoverPanicValue(site string, r any) {
	log.Printf("event=panic site=%s panic=%v stack=%q", site, r, string(debug.Stack()))
}

// safeGo launches fn on a fresh goroutine with a recover guard installed.
// The helper centralizes the recover/log pattern used by the actor's
// followup-op dispatch and the per-tick spawn / retry-timer paths, so
// site updates do not have to repeat the `defer recoverPanic(...)`
// boilerplate at every `go func()` call site.
//
// Inside the orchestrator, every spawn routes through here. Goroutines in
// other packages (runner, workspace) install their own package-local
// equivalent of recoverPanic — the orchestrator's is unexported — so the
// recover-guard rule holds package by package; `cmd/*` goroutines are
// exempt as package-main boot code.
func safeGo(site string, fn func()) {
	go func() {
		defer recoverPanic(site)
		fn()
	}()
}
