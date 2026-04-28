package tracker

import "context"

type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	URL         string
	State       string
	UpdatedAt   string
}

type Client interface {
	ListActiveIssues(ctx context.Context) ([]Issue, error)
}
