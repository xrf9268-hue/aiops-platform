package worker

// RunTaskForTest re-exports the unexported runTask orchestration so the
// external worker_test package can exercise the full lifecycle (workspace
// prep, runner, summary gate, push, PR handoff, tracker hooks) without
// promoting runTask itself to a stable public API.
var RunTaskForTest = runTask
