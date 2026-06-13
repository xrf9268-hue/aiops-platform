package runner

import (
	"log"
	"runtime/debug"
)

// recoverPanic is the runner package's process-survival guard for code on
// goroutines the runner spawns directly (the SIGKILL backstop and the
// app-server stderr drain). A panic that escaped one of those goroutines
// would otherwise crash the whole worker process — taking every unrelated
// in-flight run with it — instead of failing just the path that misbehaved
// (AGENTS.md Conventions: every non-package-main `go func` installs a
// recover guard).
//
// It is a package-local equivalent of the orchestrator's recoverPanic
// (recover.go): that one is unexported, so the runner carries its own rather
// than depending across the package boundary. The log shape matches so every
// panic site emits the same SPEC §13.1-style structured line
// (`event=panic site=<name>`) with an inline stack for diagnosis.
//
//	defer recoverPanic("runner.shell.sigkill_backstop")
func recoverPanic(site string) {
	if r := recover(); r != nil {
		log.Printf("event=panic site=%s panic=%v stack=%q", site, r, string(debug.Stack()))
	}
}
