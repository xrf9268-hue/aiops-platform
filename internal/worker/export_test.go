package worker

// RunTaskForTest re-exports the unexported runTask orchestration so the
// external worker_test package can exercise the worker lifecycle (workspace
// prep, runner, verification, summary, and secret-scan gates) without
// promoting runTask itself to a stable public API.
var RunTaskForTest = runTask
