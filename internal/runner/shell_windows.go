//go:build windows

package runner

import "os/exec"

// configurePlatformKill is a no-op on Windows; exec.CommandContext's
// default Cancel (SIGKILL on the direct child) plus cmd.WaitDelay are
// the strongest guarantees the os/exec stdlib offers cross-platform.
// Job-object based group kill could be added later if Windows worker
// hosting becomes a real target.
func configurePlatformKill(_ *exec.Cmd) {}
