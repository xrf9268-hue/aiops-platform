package workspace

import (
	"os"

	"github.com/xrf9268-hue/aiops-platform/internal/envpolicy"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// baselineHookEnvAllowlist is the always-inherited env-var allowlist for hook
// subprocesses. Operators extend it per-workflow via `hooks.env_passthrough`.
// Tracker tokens, SSH credentials, and any other secret outside this list are
// excluded by default. See docs/design/hook-verify-env-allowlist.md (#227).
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

// subprocessEnv builds the `cmd.Env` slice for a hook subprocess.
// Names from baselineHookEnvAllowlist plus passthrough are inherited from the
// worker's environment when set. Names that are not set in the worker's env
// are silently dropped so passthrough lists can include "may or may not be
// present" vars without spurious config-validation noise. Duplicate names
// across the two sources are deduplicated so `cmd.Env` does not carry the
// same `K=V` twice. Tracker credentials stay denied even when named in
// passthrough.
func subprocessEnv(passthrough []string, cfg workflow.Config) []string {
	return subprocessEnvWithLookup(passthrough, cfg, os.LookupEnv, LoginPATH)
}

func subprocessEnvWithLookup(passthrough []string, cfg workflow.Config, lookup func(string) (string, bool), loginPath func() string) []string {
	return envpolicy.BuildSanitizedEnv(baselineHookEnvAllowlist, passthrough, cfg, lookup, loginPath)
}
