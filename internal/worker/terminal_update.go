package worker

import (
	"context"
	"time"
)

// terminalUpdateContext returns a context derived from ctx that does not
// inherit cancellation. Used so that SIGTERM/SIGINT-driven shutdown does not
// leave a task wedged in `running` because the cancel-propagated UPDATE
// against tasks/task_events was rejected with `context canceled`. Capped at
// 5s so a genuinely broken DB cannot block shutdown indefinitely.
func terminalUpdateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
