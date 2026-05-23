package workspace

import "os"

// baselineHookEnvAllowlist is the always-inherited env-var allowlist for hook
// and verify subprocesses. Operators extend it per-workflow via
// `hooks.env_passthrough` / `verify.env_passthrough`. Tracker tokens, SSH
// credentials, and any other secret outside this list are excluded by default.
// See docs/design/hook-verify-env-allowlist.md (#227).
var baselineHookEnvAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TZ",
	"TERM",
}

// subprocessEnv builds the `cmd.Env` slice for a hook or verify subprocess.
// Names from baselineHookEnvAllowlist plus passthrough are inherited from the
// worker's environment when set. Names that are not set in the worker's env
// are silently dropped so passthrough lists can include "may or may not be
// present" vars without spurious config-validation noise. Duplicate names
// across the two sources are deduplicated so `cmd.Env` does not carry the
// same `K=V` twice.
func subprocessEnv(passthrough []string) []string {
	seen := make(map[string]struct{}, len(baselineHookEnvAllowlist)+len(passthrough))
	env := make([]string, 0, len(baselineHookEnvAllowlist)+len(passthrough))
	add := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		if name == "PATH" {
			// Use the once-captured login-shell PATH so the subprocess
			// inherits version-manager additions (nvm, rustup, etc.) without
			// sourcing the login profile per command — which would otherwise
			// echo profile stdout into the hook/verify output buffer (#314).
			if v := loginPATH(); v != "" {
				env = append(env, "PATH="+v)
				return
			}
		}
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for _, name := range baselineHookEnvAllowlist {
		add(name)
	}
	for _, name := range passthrough {
		add(name)
	}
	return env
}
