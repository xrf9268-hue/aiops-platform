//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// configurePlatformKill makes the shell runner survive SIGTERM-resistant
// children (e.g. `sleep` under `sh -lc`). The runner runs in its own
// process group so we can deliver signals to the entire group on
// context cancel; otherwise WaitDelay would only kill `sh` and leave
// grandchildren orphaned, blocking workspace cleanup.
func configurePlatformKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the process group leader created above.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		// Backstop: if the group is still alive after killGrace, send
		// SIGKILL to the whole group. cmd.WaitDelay only handles the
		// direct child; grandchildren can survive otherwise.
		go func(pid int) {
			time.Sleep(killGrace)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}(cmd.Process.Pid)
		return nil
	}
}
