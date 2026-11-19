package runner

import (
	"context"
	"errors"
	"time"
)

// ErrBlocked means the agent is waiting for a human: an approval dialog or a
// question. The runner never answers those, so a blocked task is failed with its
// workspace left open for someone to look at.
var ErrBlocked = errors.New("agent is waiting for input")

// Backend is where one task's work physically happens. The only implementation
// is Herdr, but the runner talks to this interface so its logic — rounds,
// scopes, judgement, recording — is testable without a session.
type Backend interface {
	// Open creates the task's worktree workspace and starts an agent in it.
	Open(ctx context.Context, req OpenRequest) (Handle, error)
	// Prompt submits the task text and returns when the agent settles, or with
	// ErrBlocked if it is waiting for a human.
	Prompt(ctx context.Context, h Handle, text string, timeout time.Duration) error
	// Read returns the agent's recent terminal output, for the record.
	Read(ctx context.Context, h Handle, lines int) (string, error)
	// Interrupt asks a working agent to stop.
	Interrupt(ctx context.Context, h Handle) error
	// Close removes the worktree and the workspace around it.
	Close(ctx context.Context, h Handle) error
	// Label is the workspace label a task's worktree carries. It is the only
	// link between a task and live session state, so it is derived, not stored.
	Label(taskID int64) string
	// TaskWorkspaces lists the task-labelled workspaces that exist now.
	TaskWorkspaces(ctx context.Context) ([]TaskWorkspace, error)
}

// OpenRequest is one attempt at one task.
type OpenRequest struct {
	TaskID         int64
	Attempt        int
	WorkspaceLabel string // the Herdr workspace the task belongs to
	Branch         string
	Base           string // ref to branch from
	AgentName      string
	Tools          []string // the role's tool set; nil leaves the backend default
}

// Handle is what exists once a task's work has been opened.
type Handle struct {
	TaskID      int64
	AgentName   string
	WorkspaceID string
	PaneID      string
	Worktree    string
	Repo        string
	Base        string // commit the branch started from
	Branch      string
}

// TaskWorkspace is a live workspace that belongs to a task.
type TaskWorkspace struct {
	TaskID      int64
	WorkspaceID string
}
