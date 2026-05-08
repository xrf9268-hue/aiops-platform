package workspace

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MirrorRoot returns the directory where bare mirror clones are cached.
//
// Resolution order:
//  1. explicit override (typically the AIOPS_MIRROR_ROOT env var) when non-empty
//  2. <user-cache-dir>/aiops-platform/mirrors (best effort)
//  3. <os.TempDir>/aiops-platform/mirrors as a last-resort fallback
//
// The directory is created on demand by callers; this function only resolves
// the path so it can be reused by Cleanup callers and tests.
func MirrorRoot(override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "aiops-platform", "mirrors")
	}
	return filepath.Join(os.TempDir(), "aiops-platform", "mirrors")
}

// mirrorPathFor maps a clone URL to a stable on-disk location under root.
// We do not need cryptographic uniqueness, just a deterministic, filesystem-
// safe path keyed by host + path so two repos never collide. The trailing
// ".git" is preserved to keep the layout recognisable when inspected by hand.
func mirrorPathFor(root, cloneURL string) string {
	host, repoPath := splitCloneURL(cloneURL)
	host = sanitize(host)
	repoPath = strings.TrimSuffix(repoPath, ".git")
	// Preserve owner/name structure so multiple repos under the same owner
	// share a parent directory, which makes manual cleanup easier.
	parts := strings.Split(repoPath, "/")
	for i, p := range parts {
		parts[i] = sanitize(p)
	}
	repoPath = strings.Join(parts, string(filepath.Separator))
	if repoPath == "" {
		repoPath = "repo"
	}
	return filepath.Join(root, host, repoPath+".git")
}

// splitCloneURL extracts a host and path component from either an https://
// URL or an scp-style ssh URL (git@host:owner/repo.git). Errors are
// swallowed; the worst case is we end up with a sanitized fallback path,
// which is still safe and deterministic.
func splitCloneURL(cloneURL string) (host, path string) {
	cloneURL = strings.TrimSpace(cloneURL)
	if cloneURL == "" {
		return "unknown", "repo"
	}
	if strings.Contains(cloneURL, "://") {
		if u, err := url.Parse(cloneURL); err == nil {
			host = u.Host
			path = strings.TrimPrefix(u.Path, "/")
			if host == "" {
				host = "unknown"
			}
			return host, path
		}
	}
	// scp-style: git@host:owner/repo.git
	if at := strings.Index(cloneURL, "@"); at >= 0 {
		rest := cloneURL[at+1:]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return rest[:colon], rest[colon+1:]
		}
	}
	return "unknown", sanitize(cloneURL)
}

// EnsureMirror returns the local path to a bare mirror clone of cloneURL,
// creating it on first use and refreshing it on subsequent calls.
//
// We deliberately do not use `git clone --mirror`: the mirror refspec
// `+refs/*:refs/*` plus a periodic `--prune` would silently delete the
// per-task work branches that worktrees depend on. Instead we use a plain
// `git clone --bare` with the default `+refs/heads/*:refs/remotes/origin/*`
// fetch refspec, which keeps remote-tracking refs neatly under
// `refs/remotes/origin/*` so our `refs/heads/<work>` branches are never
// touched by fetch --prune.
//
// First call: `git clone --bare <url> <mirror>` populates the cache.
// Subsequent calls run `git fetch --prune origin` to bring remote-tracking
// refs up to date without disturbing local work branches.
//
// The mirror parent directory is created lazily so callers do not need to
// pre-create AIOPS_MIRROR_ROOT.
func (m *Manager) EnsureMirror(ctx context.Context, cloneURL string) (string, error) {
	root := MirrorRoot(m.MirrorRoot)
	mirror := mirrorPathFor(root, cloneURL)
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		return "", fmt.Errorf("mkdir mirror parent: %w", err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err == nil {
		// Existing mirror: refresh remote-tracking refs only.
		if err := run(ctx, mirror, "git", "fetch", "--prune", "--tags", "origin"); err != nil {
			return "", fmt.Errorf("refresh mirror %s: %w", mirror, err)
		}
		return mirror, nil
	}
	// First-time clone. Remove any partial directory left over from a prior
	// failed attempt so `git clone` does not refuse to overwrite it.
	_ = os.RemoveAll(mirror)
	if err := run(ctx, filepath.Dir(mirror), "git", "clone", "--bare", cloneURL, mirror); err != nil {
		return "", fmt.Errorf("clone mirror %s: %w", cloneURL, err)
	}
	// `git clone --bare` defaults to a "do nothing" fetch refspec; rewrite
	// it to the standard remote-tracking layout so subsequent fetches and
	// `origin/<branch>` lookups behave like a regular clone.
	if err := run(ctx, mirror, "git", "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", fmt.Errorf("configure fetch refspec: %w", err)
	}
	if err := run(ctx, mirror, "git", "fetch", "--prune", "--tags", "origin"); err != nil {
		return "", fmt.Errorf("initial fetch: %w", err)
	}
	return mirror, nil
}

// Cleanup removes per-task worktrees whose directory mtime is older than
// maxAge. For each stale worktree we first try `git worktree remove --force`
// against the owning mirror so the bare repo's administrative state is
// cleaned up, then fall back to plain os.RemoveAll if git refuses (for
// example when the mirror has been deleted out from under us).
//
// The function never returns an error for a single failed entry; it logs
// the count of removed and failed entries via the returned CleanupReport so
// callers can decide whether to surface a warning. Errors are only returned
// for catastrophic conditions (root unreadable).
//
// maxAge <= 0 is treated as "remove everything", which is occasionally
// useful from an operator CLI; callers that want a no-op should skip the
// call entirely.
func (m *Manager) Cleanup(ctx context.Context, maxAge time.Duration) (CleanupReport, error) {
	report := CleanupReport{Threshold: maxAge}
	cutoff := time.Now().Add(-maxAge)
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return report, nil
		}
		return report, fmt.Errorf("read workspace root: %w", err)
	}
	for _, repoEntry := range entries {
		if !repoEntry.IsDir() {
			continue
		}
		repoDir := filepath.Join(m.Root, repoEntry.Name())
		taskEntries, err := os.ReadDir(repoDir)
		if err != nil {
			report.Failed++
			continue
		}
		for _, te := range taskEntries {
			if !te.IsDir() {
				continue
			}
			taskDir := filepath.Join(repoDir, te.Name())
			info, err := os.Stat(taskDir)
			if err != nil {
				report.Failed++
				continue
			}
			if maxAge > 0 && info.ModTime().After(cutoff) {
				report.Skipped++
				continue
			}
			if removeWorktree(ctx, taskDir) {
				report.Removed++
			} else {
				report.Failed++
			}
		}
	}
	return report, nil
}

// removeWorktree best-effort detaches a task directory from its owning bare
// mirror and deletes the on-disk path. It returns true when the directory
// is gone after the call.
func removeWorktree(ctx context.Context, taskDir string) bool {
	// The worktree's gitdir file points at <mirror>/worktrees/<id>; we ask
	// git itself to clean both sides via the worktree itself.
	_ = run(ctx, taskDir, "git", "worktree", "remove", "--force", ".")
	if _, err := os.Stat(taskDir); err == nil {
		if err := os.RemoveAll(taskDir); err != nil {
			return false
		}
	}
	return true
}

// CleanupReport summarises a Cleanup pass. It is intentionally tiny and
// JSON-friendly so it can be logged or surfaced in a future ops CLI.
type CleanupReport struct {
	Threshold time.Duration `json:"threshold"`
	Removed   int           `json:"removed"`
	Skipped   int           `json:"skipped"`
	Failed    int           `json:"failed"`
}
