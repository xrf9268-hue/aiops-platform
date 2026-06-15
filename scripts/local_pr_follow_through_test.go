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
		`worker_lock_key="$(printf '%s\n%s\n' "$workflow_path" "$workspace_root" | shasum -a 256`,
		`worker_lock_dir="${AIOPS_WORKER_LOCK_DIR:-"$HOME/Library/Caches/aiops-platform/github-worker-${worker_lock_key}.lock"}"`,
		`worker_lock_stale_seconds="${AIOPS_WORKER_LOCK_STALE_SECONDS:-3600}"`,
		`acquire_worker_lock`,
		`worker_lock_initializing`,
		`github worker already running for workflow/workspace`,
		`exec "$worker_bin" "$workflow_path"`,
		`release_worker_lock`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-github-worker.sh missing %q", want)
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
		`if [[ "$rc" -eq 124 ]]; then`,
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
	// NOT-CONFIRMED must hand off before any merge: the review_rc==20 `continue`
	// has to precede the merge call so a clean-or-not-reviewed PR is never merged.
	if strings.Index(script, `human_action_required=1`) > strings.Index(script, `gh_cmd pr merge`) {
		t.Fatal("NOT-CONFIRMED handoff (human_action_required=1) must come before the merge call")
	}
	// The 124 (timeout) branch is what records NOT-CONFIRMED.
	if strings.Index(script, `if [[ "$rc" -eq 124 ]]; then`) > strings.Index(script, `record_github_codex_not_confirmed "$pr"`) {
		t.Fatal("record_github_codex_not_confirmed must be reached from the timeout (124) branch")
	}
}
