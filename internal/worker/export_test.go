package worker

// RunTaskForTest gives the external worker_test package a handle on RunTask so
// it can exercise the worker lifecycle (workspace prep and runner). RunTask has
// no production caller — the orchestrator calls RunTaskWithResult directly — so
// this test driver is its only live use.
var RunTaskForTest = RunTask
