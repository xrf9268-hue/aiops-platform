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
	RuntimeEventCandidate RuntimeEventKind = "candidate"
	RuntimeEventRunning   RuntimeEventKind = "running"
	RuntimeEventCompleted RuntimeEventKind = "completed"
	RuntimeEventFailed    RuntimeEventKind = "failed"
	RuntimeEventBlocked   RuntimeEventKind = "blocked"
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
	Retrying     []RetryView        `json:"retrying"`
	Completed    []IssueID          `json:"completed"`
	RecentEvents []RuntimeEvent     `json:"recent_events"`
	CodexTotals  CodexTotals        `json:"codex_totals"`
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
	events := boundedRuntimeEvents(s.RecentEvents, limit)
	links := linksByIssue(events)

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
	sort.Slice(view.Retrying, func(i, j int) bool { return view.Retrying[i].IssueID < view.Retrying[j].IssueID })
	sort.Slice(view.Completed, func(i, j int) bool { return view.Completed[i] < view.Completed[j] })

	summary := StatusSummary{Running: len(running), Completed: len(view.Completed), Retrying: len(view.Retrying)}
	seenEventIssues := map[RuntimeEventKind]map[IssueID]struct{}{}
	for _, ev := range events {
		recordEventSummary(&summary, seenEventIssues, ev)
	}
	return RuntimeStatus{
		Source:       "orchestrator_runtime",
		Summary:      summary,
		Running:      running,
		Retrying:     view.Retrying,
		Completed:    view.Completed,
		RecentEvents: events,
		CodexTotals:  view.CodexTotals,
		RateLimits:   view.CodexRateLimits,
	}
}

func recordEventSummary(summary *StatusSummary, seen map[RuntimeEventKind]map[IssueID]struct{}, ev RuntimeEvent) {
	switch ev.Kind {
	case RuntimeEventCandidate, RuntimeEventFailed, RuntimeEventBlocked:
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
	case RuntimeEventBlocked:
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
