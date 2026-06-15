//go:build windows

package workspace

func acquireWorktreeOwnershipFileLock(lockPath string, nonblock bool) (func(), error) {
	return func() {}, nil
}
