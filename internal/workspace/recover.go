package workspace

import (
	"log"
	"runtime/debug"
)

// recoverPanic is the workspace package's process-survival guard for the
// goroutine that drains a hook subprocess's exit via `done <- cmd.Wait()`.
// A panic that escaped it would crash the whole worker process instead of
// failing just the hook run (AGENTS.md Conventions: every non-package-main
// `go func` installs a recover guard).
//
// It is a package-local equivalent of the orchestrator's recoverPanic
// (orchestrator/recover.go), which is unexported; the log shape matches so
// every panic site emits the same SPEC §13.1-style structured line
// (`event=panic site=<name>`) with an inline stack for diagnosis.
//
//	defer recoverPanic("workspace.hook.cmd_wait")
func recoverPanic(site string) {
	if r := recover(); r != nil {
		log.Printf("event=panic site=%s panic=%v stack=%q", site, r, string(debug.Stack()))
	}
}
