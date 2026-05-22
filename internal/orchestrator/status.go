package orchestrator

import (
	"encoding/json"
	"io"
	"sort"
	"time"
)

// RuntimeEventKind is the SPEC-aligned runtime event vocabulary used by the
// lightweight status surface. It describes orchestrator runtime state, not rows
// in the transitional queue.
type RuntimeEventKind string

const (
	RuntimeEventCandidate        RuntimeEventKind = "candidate"
	RuntimeEventRunning          RuntimeEventKind = "running"
	RuntimeEventCompleted        RuntimeEventKind = "completed"
	RuntimeEventFailed           RuntimeEventKind = "failed"
	RuntimeEventCandidateBlocked RuntimeEventKind = "blocked"
	RuntimeEventInputBlocked     RuntimeEventKind = "input_blocked"
)

// RuntimeEvent is an operator-facing event observed by the orchestrator runtime.
// Branch and PRURL are optional discoveries from agent output/events; their
// presence does not imply the worker created or pushed anything itself.
type RuntimeEvent struct {
	Kind       RuntimeEventKind `json:"kind"`
	IssueID    IssueID          `json:"issue_id,omitempty"`
	Identifier string           `json:"identifier,omitempty"`
	Message    string           `json:"message,omitempty"`
	Branch     string           `json:"branch,omitempty"`
	PRURL      string           `json:"pr_url,omitempty"`
	At         time.Time        `json:"at"`
}

type RuntimeStatus struct {
	Source       string             `json:"source"`
	Summary      StatusSummary      `json:"summary"`
	Running      []StatusRun        `json:"running"`
	Blocked      []StatusBlocked    `json:"blocked"`
	Retrying     []StatusRetry      `json:"retrying"`
	Completed    []IssueID          `json:"completed"`
	RecentEvents []RuntimeEvent     `json:"recent_events"`
	CodexTotals  StatusCodexTotals  `json:"codex_totals"`
	RateLimits   *RateLimitSnapshot `json:"rate_limits,omitempty"`
}

type StatusSummary struct {
	Candidate int `json:"candidate"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Blocked   int `json:"blocked"`
	Retrying  int `json:"retrying"`
}

type StatusRun struct {
	IssueID       IssueID   `json:"issue_id"`
	Identifier    string    `json:"identifier,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	RetryAttempt  *int      `json:"retry_attempt,omitempty"`
	WorkspacePath string    `json:"workspace_path,omitempty"`
	LastCodexAt   time.Time `json:"last_codex_at,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	PRURL         string    `json:"pr_url,omitempty"`
}

type StatusBlocked struct {
	IssueID       IssueID   `json:"issue_id"`
	Identifier    string    `json:"identifier,omitempty"`
	State         string    `json:"state,omitempty"`
	BlockedAt     time.Time `json:"blocked_at"`
	WorkspacePath string    `json:"workspace_path,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	LastCodexAt   time.Time `json:"last_codex_at,omitempty"`
	Method        string    `json:"method,omitempty"`
	Error         string    `json:"error,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	PRURL         string    `json:"pr_url,omitempty"`
}

type StatusRetry struct {
	IssueID    IssueID   `json:"issue_id"`
	Identifier string    `json:"identifier,omitempty"`
	Attempt    int       `json:"attempt"`
	DueAt      time.Time `json:"due_at"`
	Error      string    `json:"error,omitempty"`
}

type StatusCodexTotals struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

// RecordEvent appends ev to the bounded in-memory event log.
func (s *OrchestratorState) RecordEvent(ev RuntimeEvent) {
	if s == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	s.RecentEvents = append(s.RecentEvents, ev)
	const maxRuntimeEvents = 200
	if len(s.RecentEvents) > maxRuntimeEvents {
		copy(s.RecentEvents, s.RecentEvents[len(s.RecentEvents)-maxRuntimeEvents:])
		s.RecentEvents = s.RecentEvents[:maxRuntimeEvents]
	}
}

// StatusSnapshot returns a queue-independent operator status payload.
func (s *OrchestratorState) StatusSnapshot(limit int) RuntimeStatus {
	view := s.Snapshot()
	allEvents := boundedRuntimeEvents(s.RecentEvents, 0)
	events := boundedRuntimeEvents(s.RecentEvents, limit)
	links := linksByIssue(allEvents)

	running := make([]StatusRun, 0, len(view.Running))
	for _, r := range view.Running {
		row := StatusRun{
			IssueID:       r.IssueID,
			Identifier:    r.Identifier,
			StartedAt:     r.StartedAt,
			RetryAttempt:  r.RetryAttempt,
			WorkspacePath: r.WorkspacePath,
			LastCodexAt:   r.LastCodexAt,
		}
		if link, ok := links[r.IssueID]; ok {
			row.Branch = link.branch
			row.PRURL = link.prURL
		}
		running = append(running, row)
	}
	sort.Slice(running, func(i, j int) bool { return running[i].IssueID < running[j].IssueID })
	blocked := make([]StatusBlocked, 0, len(view.Blocked))
	for _, b := range view.Blocked {
		row := StatusBlocked{
			IssueID:       b.IssueID,
			Identifier:    b.Identifier,
			State:         b.State,
			BlockedAt:     b.BlockedAt,
			WorkspacePath: b.WorkspacePath,
			SessionID:     b.SessionID,
			LastCodexAt:   b.LastCodexAt,
			Method:        b.Method,
			Error:         b.Error,
		}
		if link, ok := links[b.IssueID]; ok {
			row.Branch = link.branch
			row.PRURL = link.prURL
		}
		blocked = append(blocked, row)
	}
	sort.Slice(blocked, func(i, j int) bool { return blocked[i].IssueID < blocked[j].IssueID })
	retrying := make([]StatusRetry, 0, len(view.Retrying))
	for _, r := range view.Retrying {
		retrying = append(retrying, StatusRetry{
			IssueID:    r.IssueID,
			Identifier: r.Identifier,
			Attempt:    r.Attempt,
			DueAt:      r.DueAt,
			Error:      r.Error,
		})
	}
	sort.Slice(retrying, func(i, j int) bool { return retrying[i].IssueID < retrying[j].IssueID })
	sort.Slice(view.Failed, func(i, j int) bool { return view.Failed[i] < view.Failed[j] })
	sort.Slice(view.Completed, func(i, j int) bool { return view.Completed[i] < view.Completed[j] })

	// summary.Failed reads from FailedSuppressedCount (the true
	// suppression-map size), not len(view.Failed): the latter is
	// the FIFO display slice capped at MaxRecentFailed and would
	// under-report the number of issues still being blocked by
	// IsClaimed once failure throughput crossed the cap. See the
	// carve-out in state.go's recordFailed for why the map is
	// intentionally uncapped.
	summary := StatusSummary{Running: len(running), Completed: len(view.Completed), Failed: view.FailedSuppressedCount, Blocked: len(blocked), Retrying: len(retrying)}
	seenEventIssues := map[RuntimeEventKind]map[IssueID]struct{}{}
	for _, id := range view.Failed {
		seenFailed := seenEventIssues[RuntimeEventFailed]
		if seenFailed == nil {
			seenFailed = map[IssueID]struct{}{}
			seenEventIssues[RuntimeEventFailed] = seenFailed
		}
		seenFailed[id] = struct{}{}
	}
	for _, ev := range allEvents {
		recordEventSummary(&summary, seenEventIssues, ev)
	}
	return RuntimeStatus{
		Source:       "orchestrator_runtime",
		Summary:      summary,
		Running:      running,
		Blocked:      blocked,
		Retrying:     retrying,
		Completed:    view.Completed,
		RecentEvents: events,
		CodexTotals:  statusCodexTotals(view.CodexTotals),
		RateLimits:   view.CodexRateLimits,
	}
}

func statusCodexTotals(t CodexTotals) StatusCodexTotals {
	return StatusCodexTotals{
		InputTokens:    t.InputTokens,
		OutputTokens:   t.OutputTokens,
		TotalTokens:    t.TotalTokens,
		SecondsRunning: t.SecondsRunning,
	}
}

func recordEventSummary(summary *StatusSummary, seen map[RuntimeEventKind]map[IssueID]struct{}, ev RuntimeEvent) {
	switch ev.Kind {
	case RuntimeEventCandidate, RuntimeEventFailed, RuntimeEventCandidateBlocked:
	default:
		return
	}
	if ev.IssueID != "" {
		byIssue := seen[ev.Kind]
		if byIssue == nil {
			byIssue = map[IssueID]struct{}{}
			seen[ev.Kind] = byIssue
		}
		if _, ok := byIssue[ev.IssueID]; ok {
			return
		}
		byIssue[ev.IssueID] = struct{}{}
	}
	switch ev.Kind {
	case RuntimeEventCandidate:
		summary.Candidate++
	case RuntimeEventFailed:
		summary.Failed++
	case RuntimeEventCandidateBlocked:
		summary.Blocked++
	}
}

func boundedRuntimeEvents(src []RuntimeEvent, limit int) []RuntimeEvent {
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	out := make([]RuntimeEvent, limit)
	copy(out, src[len(src)-limit:])
	return out
}

type eventLinks struct {
	branch string
	prURL  string
}

func linksByIssue(events []RuntimeEvent) map[IssueID]eventLinks {
	out := map[IssueID]eventLinks{}
	for _, ev := range events {
		if ev.IssueID == "" {
			continue
		}
		link := out[ev.IssueID]
		if ev.Branch != "" {
			link.branch = ev.Branch
		}
		if ev.PRURL != "" {
			link.prURL = ev.PRURL
		}
		out[ev.IssueID] = link
	}
	return out
}

func WriteStatusJSON(w io.Writer, status RuntimeStatus) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(status)
}
