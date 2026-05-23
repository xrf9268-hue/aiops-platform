package workspace

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// loginPATH returns the PATH value a login shell would expose to a subprocess,
// captured once per process. Hooks and verify commands use it so they inherit
// version-manager additions (nvm, rustup, pyenv, etc.) without paying the cost
// — or the stdout-capture cost — of sourcing the login profile on every command.
//
// Earned by #314: `sh -lc` re-sources `/etc/profile` (and friends) per command.
// Under dash on the default Claude Code on the web image, `/etc/profile.d/nvm.sh`
// prints "nvm\n" to stdout, which then leads every HookResult.Output and
// VerifyResult.Output. Snapshotting PATH once and running per-command shells
// without `-l` keeps the PATH-inheritance contract while removing the per-command
// profile-source side-effect.
func loginPATH() string {
	loginPATHOnce.Do(func() {
		loginPATHCached = captureLoginPATH()
	})
	return loginPATHCached
}

var (
	loginPATHOnce   sync.Once
	loginPATHCached string
)

const (
	loginPATHBegin = "__AIOPS_LOGIN_PATH_BEGIN__"
	loginPATHEnd   = "__AIOPS_LOGIN_PATH_END__"
)

// captureLoginPATH runs `sh -lc` once to read the PATH that a login shell
// would build (worker's PATH + any additions from /etc/profile.d, ~/.profile,
// etc.). The PATH is wrapped in sentinel markers in the captured stdout so
// init-script output (e.g. dash + nvm.sh printing "nvm") cannot corrupt the
// extracted value. Any failure path returns the worker's own PATH as a safe
// fallback so hooks still find common tooling.
func captureLoginPATH() string {
	fallback := os.Getenv("PATH")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	script := "printf '" + loginPATHBegin + "%s" + loginPATHEnd + "' \"$PATH\""
	cmd := exec.CommandContext(ctx, "sh", "-lc", script)
	out, err := cmd.Output()
	if err != nil {
		return fallback
	}
	s := string(out)
	begin := strings.Index(s, loginPATHBegin)
	if begin < 0 {
		return fallback
	}
	rest := s[begin+len(loginPATHBegin):]
	end := strings.Index(rest, loginPATHEnd)
	if end < 0 {
		return fallback
	}
	path := rest[:end]
	if path == "" {
		return fallback
	}
	return path
}
