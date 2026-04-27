package task

import "time"

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Task struct {
	ID            string
	Status        Status
	SourceType    string
	SourceEventID string
	RepoOwner     string
	RepoName      string
	CloneURL      string
	BaseBranch    string
	WorkBranch    string
	Title         string
	Description   string
	Actor         string
	Model         string
	Priority      int
	Attempts      int
	MaxAttempts   int
	AvailableAt   time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
