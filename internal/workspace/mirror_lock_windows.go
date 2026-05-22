//go:build windows

package workspace

// acquireMirrorFileLock is a no-op on Windows: the local LaunchAgent /
// systemd worker deployments aiops-platform supports today are
// Linux/Darwin-only (#228), and the syscall.Flock primitive used on Unix
// does not exist on Windows. The in-process sync.Mutex still serializes
// concurrent mirror operations within a single worker process; operators
// running multiple worker processes on Windows would need to layer their
// own mutual exclusion (a workflow-level cron lock, etc.).
func acquireMirrorFileLock(mirror string) (func(), error) {
	return func() {}, nil
}
