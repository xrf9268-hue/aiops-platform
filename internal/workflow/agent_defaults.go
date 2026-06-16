package workflow

// defaultAgentMaxContinuationTurns mirrors agent.max_turns into
// agent.max_continuation_turns unless the operator set the continuation cap
// explicitly in front matter, so the two SPEC §16.5 budgets default together.
func defaultAgentMaxContinuationTurns(cfg *Config, hasFrontMatter bool, front []byte) {
	if cfg == nil {
		return
	}
	if hasFrontMatter && hasNestedKey(front, "agent", "max_continuation_turns") {
		return
	}
	cfg.Agent.MaxContinuationTurns = cfg.Agent.MaxTurns
}
