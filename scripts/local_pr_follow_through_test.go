package scripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLocalPRFollowThroughClaudeReviewIsDiffOnly(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)

	for _, want := range []string{
		`local max_turns="${AIOPS_CLAUDE_REVIEW_MAX_TURNS:-6}"`,
		`--tools ""`,
		`--permission-mode bypassPermissions`,
		`--no-session-persistence`,
		`--output-format json`,
		`--json-schema "$(cat "$schema_file")"`,
		`< "$prompt_file" > "$raw_file"`,
		`jq -c '.structured_output' "$raw_file" > "$review_file"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	if strings.Contains(script, "--allowedTools") || strings.Contains(script, "--allowed-tools") {
		t.Fatal("Claude review gate must not grant repository tools; use supplied diff only")
	}
}

func TestLocalPRFollowThroughTriggersGitHubCodexReviewBeforeMerge(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`github_codex_review_timeout="${AIOPS_GITHUB_CODEX_REVIEW_TIMEOUT:-20m}"`,
		`find_existing_github_codex_review_trigger "$pr" "$head_oid" "$base_oid" "$base_ref"`,
		`audit_log "github_codex_review_existing_trigger_found"`,
		`-f body="@codex review`,
		`head: ${head_oid}`,
		`base: ${base_oid}`,
		`base_ref: ${base_ref}`,
		`wait_for_github_codex_review "$pr" "$head_oid" "$base_oid" "$base_ref"`,
		`PR #$pr head changed during GitHub Codex review`,
		`PR #$pr base changed during GitHub Codex review`,
		`GitHub Codex review trigger comment is not bound to head`,
		`[[ "$comment_body" != *"$head_oid"* || "$comment_body" != *"$base_oid"* || "$comment_body" != *"$base_ref"* ]]`,
		`pulls/${pr}/reviews?per_page=100`,
		`python3 "$signal_script" find-findings "$head_oid" "$started_at"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	if strings.Index(script, `wait_for_github_codex_review "$pr" "$head_oid" "$base_oid" "$base_ref"`) > strings.Index(script, `gh_cmd pr merge`) {
		t.Fatal("GitHub Codex review must run before merge")
	}
}

func TestLocalPRFollowThroughSerializesLaunchAgentRuns(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`follow_lock_dir="${AIOPS_FOLLOW_THROUGH_LOCK_DIR:-"$HOME/Library/Caches/aiops-platform/pr-follow-through.lock"}"`,
		`follow_lock_stale_seconds="${AIOPS_FOLLOW_THROUGH_LOCK_STALE_SECONDS:-3600}"`,
		`acquire_follow_through_lock`,
		`audit_log "lock_initializing"`,
		`follow-through already running`,
		`trap 'release_follow_through_lock' EXIT`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
}

func TestLocalPRFollowThroughBlocksDuplicatePRsForSameIssue(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`closing_issue_report_for_prs()`,
		`all_open_prs=()`,
		`pr_scan_limit="${AIOPS_PR_SCAN_LIMIT:-1000}"`,
		`gh_cmd pr list -R "$repo_path" --state open --limit "$pr_scan_limit" --json number`,
		`assert_pr_scan_not_truncated "all_open" "${all_open_prs[@]}"`,
		`audit_log "pr_scan_limit_reached"`,
		`assert_no_duplicate_closing_issue_prs "${all_open_prs[@]}"`,
		`assert_prs_claim_issues "${prs[@]}"`,
		`audit_log "duplicate_prs_detected"`,
		`audit_log "missing_pr_issue_claim"`,
		`duplicate open PRs closing the same issue`,
		`open PRs missing explicit issue claim`,
		`(?:(?:assigned|github)\s+)?issue`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
}

func TestLocalPRFollowThroughRequiresTimeoutBinary(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`timeout_bin="${AIOPS_TIMEOUT_BIN:-}"`,
		`resolve_timeout_bin()`,
		`command -v gtimeout`,
		`GNU timeout is required for auditable bounded follow-through runs`,
		`timeout_bin="$(resolve_timeout_bin)"`,
		`"$timeout_bin" "$gh_timeout" gh "$@"`,
		`"$timeout_bin" "$duration" "$@"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`else
    gh "$@"`,
		`else
    "$@"`,
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("local-pr-follow-through.sh must fail closed when timeout is unavailable; found fallback %q", forbidden)
		}
	}
}

func TestLocalPRFollowThroughRunsUncachedFileSizeGate(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)

	localGate := `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
	localRaceGate := `      go test -race -covermode=atomic ./...`
	if !strings.Contains(script, localGate) {
		t.Fatalf("local-pr-follow-through.sh missing %q", localGate)
	}
	if localGateIndex, localRaceIndex := strings.Index(script, localGate), strings.Index(script, localRaceGate); localGateIndex == -1 || localRaceIndex == -1 || localGateIndex > localRaceIndex {
		t.Fatalf("local file-size gate index = %d; want before race test index %d", localGateIndex, localRaceIndex)
	}

	dockerGate := `go test -run "^TestProductionGoFilesStayWithinSizeBudget$" -count=1 ./scripts && go test -race -covermode=atomic ./... && go build ./cmd/worker`
	if !strings.Contains(script, dockerGate) {
		t.Fatalf("local-pr-follow-through.sh missing Docker gate %q", dockerGate)
	}
}

func TestLocalPRFollowThroughCachesReviewsByHeadSHA(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`review_state_dir="${AIOPS_REVIEW_STATE_DIR:-"$HOME/Library/Caches/aiops-platform/reviews"}"`,
		`base_ref="$(jq -r '.baseRefName' <<<"$pr_refs")"`,
		`git fetch origin "+refs/heads/${base_ref}:refs/remotes/origin/${base_ref}" --quiet`,
		`fetched_base_oid="$(git rev-parse "origin/${base_ref}")"`,
		`git diff "${base_oid}...HEAD" > "$diff_file"`,
		`local_reviews_already_passed "$pr" "$head_oid" "$base_oid" "$base_ref"`,
		`local_reviews_already_failed "$pr" "$head_oid" "$base_oid" "$base_ref"`,
		`mark_local_reviews_passed "$pr" "$head_oid" "$base_oid" "$base_ref" "$artifacts_dir"`,
		`mark_local_reviews_failed "$pr" "$head_oid" "$base_oid" "$base_ref" "$claude_status" "$codex_status" "$artifacts_dir"`,
		`preserve_local_review_artifacts "$pr" "$head_oid" "$base_oid" "$base_ref"`,
		`artifacts_dir`,
		`base_ref:$base_ref`,
		`audit_log "local_reviews_cache_hit"`,
		`audit_log "local_reviews_failed_cache_hit"`,
		`audit_log "local_reviews_failed_cached"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
}

func TestLocalPRFollowThroughSkipsLocalGatesForFailedReviewCache(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	check := `if local_reviews_already_failed "$pr" "$head_oid" "$base_oid" "$base_ref"; then`
	gates := `audit_log "local_gates_started"`
	for _, want := range []string{
		check,
		`stage=before_local_gates`,
		`skipping local gates until the PR head or base changes`,
		`continue`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	if strings.Index(script, check) > strings.Index(script, gates) {
		t.Fatal("failed local review cache must be checked before expensive local gates")
	}
}

func TestLocalPRFollowThroughReusesGitHubCodexReviewTriggerByHeadSHA(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`github_codex_review_state_dir="${AIOPS_GITHUB_CODEX_REVIEW_STATE_DIR:-"$HOME/Library/Caches/aiops-platform/github-codex-review"}"`,
		`github_codex_review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref"`,
		`gh_cmd api --paginate --slurp`,
		`head_oid not in body or base_oid not in body or base_ref not in body`,
		`Reusing GitHub Codex review trigger`,
		`audit_log "github_codex_review_reused"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
}

func TestLocalPRFollowThroughUsesLongTimeoutForGitHubChecks(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`checks_timeout="${AIOPS_CHECKS_TIMEOUT:-30m}"`,
		`audit_log "github_checks_started" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref timeout=$checks_timeout"`,
		`run_with_timeout "$checks_timeout" gh pr checks -R "$repo_path" "$pr" --watch`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	if strings.Contains(script, `gh_cmd pr checks -R "$repo_path" "$pr" --watch`) {
		t.Fatal("gh pr checks --watch must not use short AIOPS_GH_TIMEOUT")
	}
}

func TestLocalPRFollowThroughRechecksHeadBeforeRemoteReview(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`current_pr_head_oid()`,
		`current_pr_ref_json()`,
		`assert_pr_refs_unchanged()`,
		`assert_local_head_matches_pr "$pr" "$head_oid"`,
		`audit_log "checkout_head_mismatch"`,
		`assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "after_local_gates"`,
		`assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "after_local_reviews"`,
		`assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "before_github_codex_review"`,
		`assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "before_github_checks"`,
		`assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "after_github_checks"`,
		`audit_log "head_changed"`,
		`audit_log "pr_refs_changed"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	if strings.Index(script, `assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "before_github_codex_review"`) > strings.Index(script, `wait_for_github_codex_review "$pr" "$head_oid" "$base_oid" "$base_ref"`) {
		t.Fatal("PR refs must be rechecked before triggering or reusing GitHub Codex review")
	}
}

func TestLocalPRFollowThroughEmitsAuditEvents(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`component=github-pr-follow-through event=%s`,
		`audit_log "pr_started"`,
		`audit_log "local_gates_started"`,
		`audit_log "local_gates_passed"`,
		`audit_log "local_reviews_started"`,
		`audit_log "github_codex_review_passed"`,
		`audit_log "github_checks_started"`,
		`audit_log "merge_requested"`,
		`audit_log "merge_skipped"`,
		`--match-head-commit "$head_oid"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
}

func TestLocalPRFollowThroughPaginatesReviewThreads(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`reviewThreads(first:100${after_clause})`,
		`pageInfo { hasNextPage endCursor }`,
		`has_next="$(jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage'`,
		`cursor="$(jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.endCursor // ""'`,
		`reviewThreads pagination reported hasNextPage without endCursor`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
}

func TestLocalPRFollowThroughPreservesReviewerFailureArtifacts(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`claude-review.raw.json`,
		`claude-review.prompt`,
		`codex-review.stdout`,
		`codex-review.prompt`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`rm -f "$schema_file" "$prompt_file" "$raw_file"`,
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("local-pr-follow-through.sh removes Claude artifacts before preservation: %q", forbidden)
		}
	}
}

func TestLocalScriptsIncludeUserLocalBinForLaunchd(t *testing.T) {
	for _, path := range []string{"local-pr-follow-through.sh", "local-github-worker.sh"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if !strings.Contains(string(body), `$HOME/.local/bin`) {
			t.Fatalf("%s PATH must include $HOME/.local/bin so launchd can find Claude Code", path)
		}
	}
}

func TestLocalGitHubWorkerUsesWorkspaceSingletonLock(t *testing.T) {
	body, err := os.ReadFile("local-github-worker.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-github-worker.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`workspace_root="${AIOPS_WORKSPACE_ROOT:-"$HOME/aiops-workspaces/github/xrf9268-hue-aiops-platform"}"`,
		`worker_lock_key="$(printf '%s\n%s\n' "$workflow_path" "$workspace_root" | shasum -a 256`,
		`worker_lock_dir="${AIOPS_WORKER_LOCK_DIR:-"$HOME/Library/Caches/aiops-platform/github-worker-${worker_lock_key}.lock"}"`,
		`worker_lock_stale_seconds="${AIOPS_WORKER_LOCK_STALE_SECONDS:-3600}"`,
		`acquire_worker_lock`,
		`worker_lock_initializing`,
		`github worker already running for workflow/workspace`,
		`export AIOPS_WORKSPACE_ROOT="$workspace_root"`,
		`exec "$worker_bin" "$workflow_path"`,
		`release_worker_lock`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-github-worker.sh missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`workspace_root="${WORKSPACE_ROOT:-`,
		`export WORKSPACE_ROOT=`,
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("local-github-worker.sh still uses removed env alias %q", forbidden)
		}
	}
	if strings.Contains(script, `"$worker_bin" "$workflow_path" &`) {
		t.Fatal("worker script must exec so the lock pid remains the worker process pid")
	}
}

func TestInstallLaunchAgentsDefaultsPRFollowThroughToAutoMerge(t *testing.T) {
	body, err := os.ReadFile("install-local-launchagents.sh")
	if err != nil {
		t.Fatalf("ReadFile(install-local-launchagents.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`follow_auto_merge="${AIOPS_AUTO_MERGE:-1}"`,
		`<key>AIOPS_AUTO_MERGE</key>`,
		`<string>${follow_auto_merge}</string>`,
		`<key>AIOPS_REVIEW_TIMEOUT</key>`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install-local-launchagents.sh missing %q", want)
		}
	}
}

// --- #870: fixture-driven, mutation-verified Codex review-completion predicates ---
//
// The bot-identity + findings classification lives in scripts/codex_review_signal.py
// (single source of truth, design D1). These tests drive that module with JSON
// fixtures so a mutation to a predicate fails an assertion, not a build.

const codexBotID int64 = 199175422

type sigUser struct {
	ID    *int64 `json:"id,omitempty"`
	Login string `json:"login,omitempty"`
	Type  string `json:"type,omitempty"`
}

type sigReview struct {
	ID          int64   `json:"id"`
	User        sigUser `json:"user"`
	CommitID    string  `json:"commit_id"`
	SubmittedAt string  `json:"submitted_at"`
}

func intPtr(v int64) *int64 { return &v }

func codexSignalPython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("python not found on PATH; codex_review_signal.py predicate tests require python")
	return ""
}

func runCodexSignal(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	py := codexSignalPython(t)
	cmd := exec.Command(py, append([]string{"codex_review_signal.py"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run codex_review_signal.py %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return strings.TrimSpace(out.String()), strings.TrimSpace(errb.String()), code
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%v): %v", v, err)
	}
	return string(b)
}

// runShellFuncRaw extracts a self-contained shell function from the script and
// invokes it in a fresh bash with the given args, returning stdout/stderr/exit.
func runShellFuncRaw(t *testing.T, script, name string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	prog := name + "() {\n" + shellFunctionBody(t, script, name) + "\n}\n" + name + " \"$@\""
	cmd := exec.Command("bash", append([]string{"-c", prog, "_"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run shell func %s: %v", name, err)
		}
		code = ee.ExitCode()
	}
	return strings.TrimSpace(out.String()), strings.TrimSpace(errb.String()), code
}

func runShellFunc(t *testing.T, script, name string, args ...string) string {
	t.Helper()
	out, errOut, code := runShellFuncRaw(t, script, name, args...)
	if code != 0 {
		t.Fatalf("shell func %s%v exit=%d stderr=%q", name, args, code, errOut)
	}
	return out
}

func TestCodexReviewSignalIdentityFailsClosedOnConflict(t *testing.T) {
	const head, trigger = "abc123head", "2026-06-15T12:00:00Z"
	fresh := "2026-06-15T12:05:00Z"
	for _, tc := range []struct {
		name     string
		user     sigUser
		wantCode int
		wantOut  string // when wantCode==0
		wantErr  string // when wantCode!=0
	}{
		{
			name:     "authoritative match",
			user:     sigUser{ID: intPtr(codexBotID), Login: "chatgpt-codex-connector[bot]", Type: "Bot"},
			wantCode: 0,
			wantOut:  "FINDINGS",
		},
		{
			name:     "login drift tolerated (id authoritative)",
			user:     sigUser{ID: intPtr(codexBotID), Login: "chatgpt-codex-connector", Type: "Bot"},
			wantCode: 0,
			wantOut:  "FINDINGS",
		},
		{
			name:     "spoof: codex login over wrong id",
			user:     sigUser{ID: intPtr(42), Login: "chatgpt-codex-connector[bot]", Type: "Bot"},
			wantCode: 3,
			wantErr:  "possible spoof",
		},
		{
			name:     "spoof: codex login with absent id",
			user:     sigUser{Login: "chatgpt-codex-connector[bot]", Type: "Bot"},
			wantCode: 3,
			wantErr:  "possible spoof",
		},
		{
			name:     "wrong type: id matches but not a Bot",
			user:     sigUser{ID: intPtr(codexBotID), Login: "chatgpt-codex-connector[bot]", Type: "User"},
			wantCode: 3,
			wantErr:  "not Bot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviews := []sigReview{{ID: 1, User: tc.user, CommitID: head, SubmittedAt: fresh}}
			out, errOut, code := runCodexSignal(t, mustJSON(t, reviews), "find-findings", head, trigger)
			if code != tc.wantCode {
				t.Fatalf("find-findings(%s) exit = %d (%s / %s); want %d", tc.name, code, out, errOut, tc.wantCode)
			}
			if tc.wantCode == 0 && !strings.HasPrefix(out, tc.wantOut) {
				t.Fatalf("find-findings(%s) stdout = %q; want prefix %q", tc.name, out, tc.wantOut)
			}
			if tc.wantCode != 0 && !strings.Contains(errOut, tc.wantErr) {
				t.Fatalf("find-findings(%s) stderr = %q; want substring %q", tc.name, errOut, tc.wantErr)
			}
		})
	}
}

func TestCodexReviewSignalFindingsBoundToHead(t *testing.T) {
	const head, trigger = "abc123head", "2026-06-15T12:00:00Z"
	codex := sigUser{ID: intPtr(codexBotID), Login: "chatgpt-codex-connector[bot]", Type: "Bot"}
	human := sigUser{ID: intPtr(42), Login: "alice", Type: "User"}
	for _, tc := range []struct {
		name    string
		reviews []sigReview
		want    string // "FINDINGS" prefix or exact "NONE"
	}{
		{
			name:    "codex review of current head after trigger",
			reviews: []sigReview{{ID: 10, User: codex, CommitID: head, SubmittedAt: "2026-06-15T12:05:00Z"}},
			want:    "FINDINGS",
		},
		{
			name:    "codex review of a different head is ignored",
			reviews: []sigReview{{ID: 11, User: codex, CommitID: "OTHERhead", SubmittedAt: "2026-06-15T12:05:00Z"}},
			want:    "NONE",
		},
		{
			name:    "codex review older than the trigger is ignored",
			reviews: []sigReview{{ID: 12, User: codex, CommitID: head, SubmittedAt: "2026-06-15T11:00:00Z"}},
			want:    "NONE",
		},
		{
			name:    "human review on the head does not count",
			reviews: []sigReview{{ID: 13, User: human, CommitID: head, SubmittedAt: "2026-06-15T12:05:00Z"}},
			want:    "NONE",
		},
		{
			name: "mixed set: stale codex + human + fresh codex",
			reviews: []sigReview{
				{ID: 14, User: codex, CommitID: head, SubmittedAt: "2026-06-15T11:00:00Z"},
				{ID: 15, User: human, CommitID: head, SubmittedAt: "2026-06-15T12:05:00Z"},
				{ID: 16, User: codex, CommitID: head, SubmittedAt: "2026-06-15T12:06:00Z"},
			},
			want: "FINDINGS",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := runCodexSignal(t, mustJSON(t, tc.reviews), "find-findings", head, trigger)
			if code != 0 {
				t.Fatalf("find-findings(%s) exit = %d (%s)", tc.name, code, errOut)
			}
			if tc.want == "NONE" && out != "NONE" {
				t.Fatalf("find-findings(%s) = %q; want NONE", tc.name, out)
			}
			if tc.want == "FINDINGS" && !strings.HasPrefix(out, "FINDINGS") {
				t.Fatalf("find-findings(%s) = %q; want FINDINGS", tc.name, out)
			}
		})
	}
}

func TestCodexReviewSignalAcceptsPaginatedSlurpShape(t *testing.T) {
	const head, trigger = "abc123head", "2026-06-15T12:00:00Z"
	codex := sigUser{ID: intPtr(codexBotID), Login: "chatgpt-codex-connector[bot]", Type: "Bot"}
	// gh api --paginate --slurp yields a list of per-page lists.
	pages := [][]sigReview{{{ID: 20, User: codex, CommitID: head, SubmittedAt: "2026-06-15T12:05:00Z"}}}
	out, errOut, code := runCodexSignal(t, mustJSON(t, pages), "find-findings", head, trigger)
	if code != 0 || !strings.HasPrefix(out, "FINDINGS") {
		t.Fatalf("find-findings(paginated) = %q exit=%d (%s); want FINDINGS exit 0", out, code, errOut)
	}
}

func TestCodexReviewSignalPinsBotIdentityInOnePlace(t *testing.T) {
	mod, err := os.ReadFile("codex_review_signal.py")
	if err != nil {
		t.Fatalf("ReadFile(codex_review_signal.py): %v", err)
	}
	for _, want := range []string{
		"CODEX_BOT_ID = 199175422",
		`CODEX_BOT_LOGIN = "chatgpt-codex-connector[bot]"`,
	} {
		if !strings.Contains(string(mod), want) {
			t.Fatalf("codex_review_signal.py missing %q", want)
		}
	}
	// Single source of truth: the shell script must not re-hardcode the numeric
	// id (it delegates to the helper). A stray literal here is the #870 trap.
	shell, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	if strings.Contains(string(shell), "199175422") {
		t.Fatal("local-pr-follow-through.sh hardcodes the Codex bot id; keep it only in codex_review_signal.py")
	}
}

func TestCodexReviewSignalShellGuardFailsClosed(t *testing.T) {
	// An identity conflict makes the helper exit 3; the poll loop must fail
	// closed regardless of bash version (errexit-on-`v=$(…)` only fires on bash
	// >= 4.4). Drive the REAL helper through the same `if ! classification=$(…)`
	// guard the script uses and assert the shell aborts (exit 1), not falls
	// through to keep polling.
	py := codexSignalPython(t)
	spoof := mustJSON(t, []sigReview{{
		ID:          1,
		User:        sigUser{ID: intPtr(42), Login: "chatgpt-codex-connector[bot]", Type: "Bot"},
		CommitID:    "head",
		SubmittedAt: "2026-06-15T12:05:00Z",
	}})
	guard := `set -euo pipefail
if ! classification="$(printf "%s" "$REVIEWS" | "$PY" codex_review_signal.py find-findings head 2026-06-15T12:00:00Z)"; then
  echo CONFLICT >&2
  exit 1
fi
printf '%s' "$classification"`
	cmd := exec.Command("bash", "-c", guard)
	cmd.Env = append(os.Environ(), "REVIEWS="+spoof, "PY="+py)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("spoof guard: want non-zero exit, got err=%v stdout=%q", err, out.String())
	}
	if ee.ExitCode() != 1 {
		t.Fatalf("spoof guard exit = %d (stdout=%q stderr=%q); want 1 (fail-closed)", ee.ExitCode(), out.String(), errb.String())
	}
	if strings.Contains(out.String(), "FINDINGS") {
		t.Fatalf("spoof guard leaked FINDINGS through a fail-closed path: %q", out.String())
	}
	// Pin that the script actually uses the version-independent guard form.
	script, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	if !strings.Contains(string(script), `if ! classification="$(printf "%s" "$reviews_json" | python3 "$signal_script" find-findings "$head_oid" "$started_at")"; then`) {
		t.Fatal("poll loop must guard the find-findings substitution with `if ! classification=$(...)` so a spoof fails closed regardless of bash version")
	}
}

func TestLocalPRFollowThroughHandsNotConfirmedToHuman(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`github_codex_not_confirmed_state_dir="${AIOPS_GITHUB_CODEX_NOT_CONFIRMED_STATE_DIR:-`,
		`if [[ -f "$not_confirmed_file" ]]; then`,
		`record_github_codex_not_confirmed "$pr" "$head_oid" "$base_oid" "$base_ref" "$trigger_id"`,
		`audit_log "github_codex_review_not_confirmed"`,
		`action=human_review_required`,
		`open_threads=${open_threads:-none}`,
		// NOT-CONFIRMED rides the inner loop's own no-signal deadline (sentinel
		// 75), NOT the outer timeout 124 — a hung gh/network call yields 124 and
		// must stay a retryable hard error, not a suppressing handoff (codex #879).
		`if (( budget_seconds > 0 )) && (( SECONDS >= budget_seconds )); then`,
		`exit 75`,
		`if [[ "$rc" -eq 75 ]]; then`,
		`hard_cap_arg="$((poll_budget_seconds + 120))s"`,
		`run_with_timeout "$hard_cap_arg" bash -c`,
		`return 20`,
		`wait_for_github_codex_review "$pr" "$head_oid" "$base_oid" "$base_ref" || review_rc=$?`,
		`if [[ "$review_rc" -eq 20 ]]; then`,
		`human_action_required=1`,
		`exit 20`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	// Timeout 124 must NOT be the NOT-CONFIRMED trigger (that is the clean
	// deadline sentinel 75); 124 means a command hung and the head stays
	// retryable. Guard against a regression that conflates them again.
	if strings.Contains(script, `if [[ "$rc" -eq 124 ]]; then`) {
		t.Fatal("rc==124 (a hung command / outer timeout) must stay a hard error, not the NOT-CONFIRMED path; use the 75 clean-deadline sentinel")
	}
	// NOT-CONFIRMED must hand off before any merge: the review_rc==20 `continue`
	// has to precede the merge call so a clean-or-not-reviewed PR is never merged.
	if strings.Index(script, `human_action_required=1`) > strings.Index(script, `gh_cmd pr merge`) {
		t.Fatal("NOT-CONFIRMED handoff (human_action_required=1) must come before the merge call")
	}
	// The clean-deadline (75) branch is what records NOT-CONFIRMED.
	if strings.Index(script, `if [[ "$rc" -eq 75 ]]; then`) > strings.Index(script, `record_github_codex_not_confirmed "$pr"`) {
		t.Fatal("record_github_codex_not_confirmed must be reached from the clean-deadline (75) branch")
	}
}

func TestLocalPRFollowThroughDurationToSeconds(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	if !strings.Contains(script, `duration_to_seconds() {`) {
		t.Fatal("local-pr-follow-through.sh missing duration_to_seconds helper")
	}
	// duration_to_seconds is a best-effort optimizer: it reduces the COMMON
	// decimal forms (incl. the GNU/strtod float spellings the old code passed
	// straight through) to whole seconds for the inner deadline.
	for _, tc := range []struct {
		in, want string
	}{
		{"1200", "1200"},
		{"20m", "1200"},
		{"1h", "3600"},
		{"30s", "30"},
		{"0.5m", "30"},
		{"1.5h", "5400"},
		{"0.4s", "1"}, // ceil to whole seconds
		{".5m", "30"},
		{"1.", "1"},
		{"1e1s", "10"},
		{"+10s", "10"},
	} {
		out := runShellFunc(t, script, "duration_to_seconds", tc.in)
		if out != tc.want {
			t.Fatalf("duration_to_seconds(%q) = %q; want %q", tc.in, out, tc.want)
		}
	}
	// Everything it can't reduce — `0` (disables), exotic GNU spellings
	// (inf, 0x1p3s), and outright garbage — prints EMPTY and exits 0; the helper
	// is not the validator. The caller hands these to GNU `timeout` (which
	// disables on 0, bounds on inf/hex, and errors on garbage) and skips the
	// inner deadline. This is the fix for the #879 P3 "rejected valid GNU
	// spellings with exit 2" regression — it must not fail closed here.
	for _, in := range []string{"0", "0m", "inf", "0x1p3s", "notaduration", "-5s", "1..2", "m", ""} {
		out, _, code := runShellFuncRaw(t, script, "duration_to_seconds", in)
		if code != 0 || out != "" {
			t.Fatalf("duration_to_seconds(%q) = %q exit=%d; want empty/0 (delegated to GNU timeout)", in, out, code)
		}
	}
	// The non-decimal branch must hand the RAW value to the outer timeout (GNU is
	// the validator) with budget 0 (no inner deadline); the inner loop only
	// self-deadlines when budget > 0, and caps the sleep to the remaining budget.
	for _, want := range []string{
		`if [[ -z "$poll_budget_seconds" ]]; then`,
		`hard_cap_arg="$github_codex_review_timeout"`,
		`budget_arg=0`,
		`if (( budget_seconds > 0 )) && (( SECONDS >= budget_seconds )); then`,
		`if (( budget_seconds > 0 && budget_seconds - SECONDS < sleep_for )); then`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
}

// Drives the REAL recheck_suppressed_codex_findings helper against fixture
// reviews JSON (gh_cmd stubbed). It must return 0 only when a Codex FINDINGS
// review object for the head landed after started_at — the late-review case the
// suppression re-check exists to catch (#894) — and non-zero (keep the
// suppression, hand to a human) on no review, a different head, or a review
// older than the trigger. Mutation: gut the find-findings call and the
// FINDINGS/NONE cases diverge.
func TestLocalPRFollowThroughRecheckSuppressedFindings(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found on PATH; the shell helper invokes python3 directly")
	}
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	fnBody := shellFunctionBody(t, string(body), "recheck_suppressed_codex_findings")
	const head, started = "abc123head", "2026-06-15T12:00:00Z"
	codex := sigUser{ID: intPtr(codexBotID), Login: "chatgpt-codex-connector[bot]", Type: "Bot"}
	human := sigUser{ID: intPtr(42), Login: "alice", Type: "User"}
	spoof := sigUser{ID: intPtr(42), Login: "chatgpt-codex-connector[bot]", Type: "Bot"}
	for _, tc := range []struct {
		name     string
		reviews  []sigReview
		wantCode int // 0 => clear suppression; 3 => identity-conflict hard error; other non-zero => keep suppression
	}{
		{"findings landed late", []sigReview{{ID: 10, User: codex, CommitID: head, SubmittedAt: "2026-06-15T12:05:00Z"}}, 0},
		{"still no review", []sigReview{}, 1},
		{"review of a different head", []sigReview{{ID: 11, User: codex, CommitID: "OTHERhead", SubmittedAt: "2026-06-15T12:05:00Z"}}, 1},
		{"review older than the trigger", []sigReview{{ID: 12, User: codex, CommitID: head, SubmittedAt: "2026-06-15T11:00:00Z"}}, 1},
		{"human review of head is not codex findings", []sigReview{{ID: 13, User: human, CommitID: head, SubmittedAt: "2026-06-15T12:05:00Z"}}, 1},
		// A spoofed Codex login (wrong id) must propagate find-findings' exit 3
		// as a distinct hard error, NOT collapse into the NONE keep-suppression
		// path — so the caller hard-fails like the poll loop. (#894 / PR #903 P2)
		{"identity conflict (spoofed codex login)", []sigReview{{ID: 14, User: spoof, CommitID: head, SubmittedAt: "2026-06-15T12:05:00Z"}}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prog := "set -euo pipefail\n" +
				"repo_root=\"$(dirname \"$PWD\")\"\n" +
				"repo_owner=o\nrepo_name=n\n" +
				"gh_cmd() { printf '%s' \"$REVIEWS\"; }\n" +
				"recheck_suppressed_codex_findings() {\n" + fnBody + "\n}\n" +
				"recheck_suppressed_codex_findings 7 " + head + " " + started + "\n"
			cmd := exec.Command("bash", "-c", prog)
			cmd.Env = append(os.Environ(), "REVIEWS="+mustJSON(t, tc.reviews))
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			code := 0
			if runErr := cmd.Run(); runErr != nil {
				var ee *exec.ExitError
				if !errors.As(runErr, &ee) {
					t.Fatalf("run recheck helper: %v (stderr=%q)", runErr, errb.String())
				}
				code = ee.ExitCode()
			}
			if code != tc.wantCode {
				t.Fatalf("recheck_suppressed_codex_findings(%s) exit=%d; want %d (stderr=%q)",
					tc.name, code, tc.wantCode, errb.String())
			}
		})
	}
}

// Pins the wiring that makes the late-review re-check effective: the suppression
// branch reads started_at, re-checks, clears the state file and returns 0 via
// the FINDINGS path on a hit; record_github_codex_not_confirmed persists
// started_at and its caller passes it. Deleting the re-check wiring fails here.
func TestLocalPRFollowThroughClearsSuppressionOnLateFindings(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		// state file now carries started_at, and the caller supplies it.
		`local pr="$1" head_oid="$2" base_oid="$3" base_ref="$4" trigger_id="$5" started_at="$6"`,
		`started_at:$started_at,`,
		`record_github_codex_not_confirmed "$pr" "$head_oid" "$base_oid" "$base_ref" "$trigger_id" "$started_at"`,
		// suppression branch re-checks before honoring the suppression, and
		// distinguishes the FINDINGS (clear) / identity-conflict (hard error) /
		// NONE (keep) outcomes by the helper's exit code.
		`nc_started_at="$(jq -r '.started_at // empty' "$not_confirmed_file" 2>/dev/null || true)"`,
		`recheck_suppressed_codex_findings "$pr" "$head_oid" "$nc_started_at" || recheck_rc=$?`,
		`if [[ "$recheck_rc" -eq 0 ]]; then`,
		`rm -f "$not_confirmed_file"`,
		`audit_log "github_codex_review_not_confirmed_cleared"`,
		`action=findings_landed_late`,
		// a spoof (find-findings exit 3) is a hard error, like the poll loop —
		// never folded into the benign keep-suppression/exit-20 path.
		`elif [[ "$recheck_rc" -eq 3 ]]; then`,
		`audit_log "github_codex_review_recheck_identity_conflict"`,
		`action=hard_error`,
		// one API call; the helper propagates find-findings' exit code so the
		// conflict code 3 stays distinct from NONE; FINDINGS-only success.
		`reviews_json="$(gh_cmd api --paginate --slurp "repos/${repo_owner}/${repo_name}/pulls/${pr}/reviews?per_page=100")" || return 1`,
		`classification="$(printf "%s" "$reviews_json" | python3 "$signal_script" find-findings "$head_oid" "$started_at")" || rc=$?`,
		`if [[ "$rc" -ne 0 ]]; then`,
		`[[ "$classification" == FINDINGS* ]]`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	// The cleared path must reach the all-thread gate then return 0, and must do
	// so without re-entering the poll loop (no second run_with_timeout between
	// the re-check and its return 0).
	clearIdx := strings.Index(script, `audit_log "github_codex_review_not_confirmed_cleared"`)
	threadIdx := strings.Index(script, `assert_no_actionable_threads "$pr"`)
	if clearIdx < 0 || threadIdx < clearIdx {
		t.Fatal("cleared suppression must fall through to assert_no_actionable_threads then return 0")
	}
}
