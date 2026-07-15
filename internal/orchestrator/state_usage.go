package orchestrator

import "time"

type BudgetGuardrails struct {
	MaxTokensPerClaim         int64
	MaxRuntimeSecondsPerClaim int64
}

type SessionUsageEntry struct {
	IssueID        IssueID
	Identifier     string
	IssueURL       string
	State          string
	Session        LiveSession
	Tokens         TokensView
	RuntimeSeconds float64
	CompletedAt    time.Time
	Outcome        string
}

func (s *OrchestratorState) finishRunAborted(id IssueID, run *RunningEntry, elapsed time.Duration, outcome string) bool {
	if current, ok := s.Running[id]; !ok || current != run {
		return false
	}
	s.recordEndedSessionUsage(id, run, elapsed, time.Now().UTC(), outcome)
	delete(s.Running, id)
	delete(s.Claimed, id)
	delete(s.ClaimedIssues, id)
	s.CodexTotals.AddSeconds(elapsed)
	return true
}

func (s *OrchestratorState) recordEndedSessionUsage(id IssueID, run *RunningEntry, elapsed time.Duration, completedAt time.Time, outcome string) {
	if s == nil || run == nil {
		return
	}
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	entry := SessionUsageEntry{
		IssueID:        id,
		Identifier:     run.Identifier,
		IssueURL:       run.Issue.URL,
		State:          run.Issue.State,
		Session:        run.Session,
		Tokens:         tokensViewFromRunningEntry(run),
		RuntimeSeconds: elapsedSeconds(elapsed),
		CompletedAt:    completedAt,
		Outcome:        outcome,
	}
	s.endedSessionUsage = append(s.endedSessionUsage, entry)
	if s.MaxRecentCompleted > 0 && len(s.endedSessionUsage) > s.MaxRecentCompleted {
		copy(s.endedSessionUsage, s.endedSessionUsage[len(s.endedSessionUsage)-s.MaxRecentCompleted:])
		s.endedSessionUsage = s.endedSessionUsage[:s.MaxRecentCompleted]
	}
}

func tokensViewFromRunningEntry(run *RunningEntry) TokensView {
	if run == nil {
		return TokensView{}
	}
	return TokensView{
		InputTokens:  run.CodexInputTokens,
		OutputTokens: run.CodexOutputTokens,
		TotalTokens:  run.CodexTotalTokens,
	}
}

func elapsedSeconds(elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return elapsed.Seconds()
}
