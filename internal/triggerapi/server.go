package triggerapi

import (
	"context"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// Store is the subset of the queue store the trigger API needs.
type Store interface {
	Enqueue(context.Context, task.Task) (task.Task, bool, error)
	GetTask(context.Context, string) (task.Task, error)
	ListTasks(context.Context, task.Status) ([]task.Task, error)
	TaskEvents(context.Context, string) ([]task.Event, error)
}

type Server struct {
	store  Store
	secret string
}

func NewServer(store Store, secret string) *Server {
	return &Server{store: store, secret: secret}
}
