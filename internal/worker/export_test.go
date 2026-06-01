package worker

// RunTaskForTest re-exports the unexported runTask orchestration so the
// external worker_test package can exercise the worker lifecycle (workspace
// prep, runner, policy, and RUN_SUMMARY gates) without promoting runTask
// itself to a stable public API.
var RunTaskForTest = RunTask
