package orchestrator

import (
	"context"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestRecordRuntimeEventAbsoluteUsageIgnoresGenericNotification(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-ABSOLUTE-USAGE")
	defer cancel()

	events := []task.RuntimeEvent{
		absoluteUsageEvent(100, 20, 120),
		{
			Event: task.EventNotification,
			Payload: map[string]any{
				"usage": map[string]any{"input_tokens": 5, "output_tokens": 1, "total_tokens": 6},
			},
		},
		absoluteUsageEvent(100, 20, 120),
	}
	for _, event := range events {
		if err := o.RecordRuntimeEvent(context.Background(), issueID, event); err != nil {
			t.Fatalf("RecordRuntimeEvent(%q) = %v; want nil", event.Event, err)
		}
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() = %v; want nil", err)
	}
	if len(view.Running) != 1 {
		t.Fatalf("Running rows = %d; want 1", len(view.Running))
	}
	want := TokensView{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}
	if got := view.Running[0].Tokens; got != want {
		t.Fatalf("Running tokens = %+v; want %+v", got, want)
	}
	if got := view.CodexTotals; got.InputTokens != 100 || got.OutputTokens != 20 || got.TotalTokens != 120 {
		t.Fatalf("CodexTotals = %+v; want input=100 output=20 total=120", got)
	}
}

func TestRecordRuntimeEventAbsoluteUsageAddsOnlyMonotonicIncrease(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-MONOTONIC-USAGE")
	defer cancel()

	for _, event := range []task.RuntimeEvent{
		absoluteUsageEvent(8, 3, 11),
		absoluteUsageEvent(8, 3, 11),
		absoluteUsageEvent(10, 4, 14),
	} {
		if err := o.RecordRuntimeEvent(context.Background(), issueID, event); err != nil {
			t.Fatalf("RecordRuntimeEvent(%q) = %v; want nil", event.Event, err)
		}
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() = %v; want nil", err)
	}
	if got := view.CodexTotals; got.InputTokens != 10 || got.OutputTokens != 4 || got.TotalTokens != 14 {
		t.Fatalf("CodexTotals = %+v; want input=10 output=4 total=14", got)
	}
}

func TestRecordRuntimeEventDoesNotDeriveMissingProviderTotal(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-EXPLICIT-TOTAL")
	defer cancel()

	events := []task.RuntimeEvent{
		absoluteUsageEvent(100, 20, 120),
		{
			Event: task.EventNotification,
			Payload: map[string]any{"token_usage": map[string]any{
				"total": map[string]any{"input_tokens": 110, "output_tokens": 30},
			}},
		},
	}
	for _, event := range events {
		if err := o.RecordRuntimeEvent(context.Background(), issueID, event); err != nil {
			t.Fatalf("RecordRuntimeEvent(%q) = %v; want nil", event.Event, err)
		}
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() = %v; want nil", err)
	}
	want := TokensView{InputTokens: 110, OutputTokens: 30, TotalTokens: 120}
	if got := view.Running[0].Tokens; got != want {
		t.Fatalf("Running tokens = %+v; want explicit provider totals %+v", got, want)
	}
	if got := view.CodexTotals; got.InputTokens != 110 || got.OutputTokens != 30 || got.TotalTokens != 120 {
		t.Fatalf("CodexTotals = %+v; want input=110 output=30 explicit total=120", got)
	}
}

func TestRecordRuntimeEventIgnoresLastTokenUsage(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-LAST-USAGE")
	defer cancel()

	events := []task.RuntimeEvent{
		{
			Event: task.EventNotification,
			Payload: map[string]any{"token_usage": map[string]any{
				"last":  map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
				"total": map[string]any{"input_tokens": 200, "output_tokens": 100, "total_tokens": 300},
			}},
		},
		{
			Event: task.EventNotification,
			Payload: map[string]any{"token_usage": map[string]any{
				"last": map[string]any{"input_tokens": 8, "output_tokens": 3, "total_tokens": 11},
			}},
		},
	}
	for _, event := range events {
		if err := o.RecordRuntimeEvent(context.Background(), issueID, event); err != nil {
			t.Fatalf("RecordRuntimeEvent(%q) = %v; want nil", event.Event, err)
		}
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() = %v; want nil", err)
	}
	if got := view.CodexTotals; got.InputTokens != 200 || got.OutputTokens != 100 || got.TotalTokens != 300 {
		t.Fatalf("CodexTotals = %+v; want input=200 output=100 total=300", got)
	}
}

func TestApplyTokenUsageUsesLastReportedAbsoluteBaseline(t *testing.T) {
	run := &RunningEntry{
		CodexInputTokens:         105,
		CodexOutputTokens:        21,
		CodexTotalTokens:         126,
		LastReportedInputTokens:  100,
		LastReportedOutputTokens: 20,
		LastReportedTotalTokens:  120,
	}
	usage := tokenUsage{
		input: 110, output: 22, total: 132,
		hasInput: true, hasOutput: true, hasTotal: true,
	}

	inputDelta, outputDelta, totalDelta := applyTokenUsage(run, usage)
	if inputDelta != 10 || outputDelta != 2 || totalDelta != 12 {
		t.Fatalf("applyTokenUsage deltas = %d/%d/%d; want 10/2/12 from last absolute report", inputDelta, outputDelta, totalDelta)
	}
	if run.CodexInputTokens != 110 || run.CodexOutputTokens != 22 || run.CodexTotalTokens != 132 {
		t.Fatalf("running tokens = %d/%d/%d; want authoritative 110/22/132", run.CodexInputTokens, run.CodexOutputTokens, run.CodexTotalTokens)
	}
	if run.LastReportedInputTokens != 110 || run.LastReportedOutputTokens != 22 || run.LastReportedTotalTokens != 132 {
		t.Fatalf("last reported tokens = %d/%d/%d; want 110/22/132", run.LastReportedInputTokens, run.LastReportedOutputTokens, run.LastReportedTotalTokens)
	}
}

func absoluteUsageEvent(input, output, total int64) task.RuntimeEvent {
	return task.RuntimeEvent{
		Event: task.EventNotification,
		Payload: map[string]any{
			"token_usage": map[string]any{
				"total": map[string]any{
					"input_tokens": input, "output_tokens": output, "total_tokens": total,
				},
			},
		},
	}
}
