package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"loa/internal/herdr"
)

// Herdr is the real backend: the worktree, the pane and the agent all belong to
// the session the runner was started in.
type Herdr struct {
	Client *herdr.Client
	// AgentKind is the executable Herdr starts ("pi", "claude", ...).
	AgentKind string
	// AgentArgs are the agent's own arguments, e.g. its tool allowlist.
	AgentArgs []string
	// ReadyTimeout bounds how long a freshly created pane may take to accept an
	// agent.
	ReadyTimeout time.Duration
}

// Label is the workspace label a task's worktree carries.
func (h *Herdr) Label(taskID int64) string {
	return herdr.TaskLabel(taskID)
}

// Open resolves the task's workspace, creates the worktree, and starts the
// agent. Failure at any step leaves nothing behind.
func (h *Herdr) Open(ctx context.Context, req OpenRequest) (Handle, error) {
	workspaceID, err := h.resolve(ctx, req.WorkspaceLabel)
	if err != nil {
		return Handle{}, err
	}

	created, err := h.createWorktree(ctx, workspaceID, req, req.Base)
	usedBase := req.Base
	// Herdr, not the runner, resolves the base ref, so a missing dependency
	// branch only shows up as a failed create. Branching from the configured
	// base instead is what keeps a merged-and-tidied dependency from costing
	// the dependent task an attempt.
	if err != nil && req.FallbackBase != "" && req.FallbackBase != req.Base {
		var fallbackErr error
		created, fallbackErr = h.createWorktree(ctx, workspaceID, req, req.FallbackBase)
		if fallbackErr != nil {
			return Handle{}, fmt.Errorf("%w (and from %s: %v)", err, req.FallbackBase, fallbackErr)
		}
		usedBase = req.FallbackBase
		err = nil
	}
	if err != nil {
		return Handle{}, err
	}

	// The branch starts at the base commit, so HEAD is that commit right now.
	// Reading it back is how the runner knows what "one commit ahead" means.
	base, err := gitOutput(ctx, created.Path, "rev-parse", "HEAD^{commit}")
	if err != nil {
		_ = h.Client.RemoveWorktree(ctx, created.WorkspaceID)
		return Handle{}, fmt.Errorf("resolve base: %w", err)
	}

	handle := Handle{
		TaskID:      req.TaskID,
		AgentName:   req.AgentName,
		WorkspaceID: created.WorkspaceID,
		PaneID:      created.PaneID,
		Worktree:    created.Path,
		Repo:        created.Repo,
		Base:        base,
		BaseRef:     usedBase,
		Branch:      req.Branch,
	}

	if err := h.startAgent(ctx, herdr.StartAgentRequest{
		Name:    req.AgentName,
		Kind:    h.agentKind(),
		PaneID:  created.PaneID,
		Args:    h.agentArgs(req.Tools),
		Timeout: h.readyTimeout(),
	}); err != nil {
		// A workspace without a working agent is garbage the human would have to
		// close by hand.
		_ = h.Client.RemoveWorktree(ctx, created.WorkspaceID)
		return Handle{}, fmt.Errorf("start agent: %w", err)
	}
	return handle, nil
}

// startAgent retries while the root pane is not yet at a prompt: a worktree
// workspace is created and its shell has to come up before an agent can be
// started in it.
func (h *Herdr) startAgent(ctx context.Context, req herdr.StartAgentRequest) error {
	deadline := time.Now().Add(h.readyTimeout())
	for {
		err := h.Client.StartAgent(ctx, req)
		if err == nil {
			return nil
		}
		if herdr.CodeOf(err) != "agent_not_ready" || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// createWorktree asks Herdr for one worktree, naming the ref it branches from.
func (h *Herdr) createWorktree(ctx context.Context, workspaceID string, req OpenRequest, base string) (herdr.Worktree, error) {
	created, err := h.Client.CreateWorktree(ctx, herdr.WorktreeRequest{
		WorkspaceID: workspaceID,
		Branch:      req.Branch,
		Base:        base,
		Label:       h.Label(req.TaskID),
	})
	if err != nil {
		return herdr.Worktree{}, fmt.Errorf("create worktree: %w", err)
	}
	return created, nil
}

// resolve turns a workspace label into an id. Exact match only: a task that
// names the wrong thing fails here rather than working in the wrong repository.
func (h *Herdr) resolve(ctx context.Context, label string) (string, error) {
	workspaces, err := h.Client.Workspaces(ctx)
	if err != nil {
		return "", err
	}

	var matches []herdr.Workspace
	for _, w := range workspaces {
		if w.Label == label {
			matches = append(matches, w)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no Herdr workspace is labelled %q", label)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%d Herdr workspaces are labelled %q", len(matches), label)
	}
}

// Prompt reports a blocked agent as ErrBlocked so the runner can leave the pane
// for the human instead of tearing it down.
func (h *Herdr) Prompt(ctx context.Context, handle Handle, text string, timeout time.Duration) error {
	err := h.Client.Prompt(ctx, handle.AgentName, text, timeout)
	if herdr.CodeOf(err) == "agent_blocked" {
		return fmt.Errorf("%w: %w", ErrBlocked, err)
	}
	return err
}

// Read returns the agent's terminal output, which is the record of the attempt.
func (h *Herdr) Read(ctx context.Context, handle Handle, lines int) (string, error) {
	return h.Client.Read(ctx, handle.AgentName, lines)
}

// Interrupt asks the agent to stop. The worktree is deleted right after, so
// this is the last chance for a worker to flush anything.
func (h *Herdr) Interrupt(ctx context.Context, handle Handle) error {
	return h.Client.SendKeys(ctx, handle.AgentName, "ctrl+c")
}

// Close removes the worktree and, with it, the workspace Herdr opened around it.
func (h *Herdr) Close(ctx context.Context, handle Handle) error {
	if handle.WorkspaceID == "" {
		return nil
	}
	if err := h.Client.RemoveWorktree(ctx, handle.WorkspaceID); err != nil {
		// The checkout may be gone already. Closing the workspace still removes
		// the pane the human would otherwise keep seeing.
		if closeErr := h.Client.CloseWorkspace(ctx, handle.WorkspaceID); closeErr != nil {
			return fmt.Errorf("remove worktree: %w; close workspace: %w", err, closeErr)
		}
	}
	return nil
}

// TaskWorkspaces lists every workspace carrying a task label.
func (h *Herdr) TaskWorkspaces(ctx context.Context) ([]TaskWorkspace, error) {
	workspaces, err := h.Client.Workspaces(ctx)
	if err != nil {
		return nil, err
	}

	var out []TaskWorkspace
	for _, w := range workspaces {
		if id, ok := herdr.ParseTaskLabel(w.Label); ok {
			out = append(out, TaskWorkspace{TaskID: id, WorkspaceID: w.ID})
		}
	}
	return out, nil
}

// agentArgs applies a role's tool set to the configured arguments. A nil set
// keeps the backend's own arguments, so a caller that does not know about roles
// behaves as before.
func (h *Herdr) agentArgs(tools []string) []string {
	args := append([]string(nil), h.AgentArgs...)
	if len(tools) == 0 {
		return args
	}
	joined := strings.Join(tools, ",")
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--tools" {
			args[i+1] = joined
			return args
		}
	}
	return append(args, "--tools", joined)
}

func (h *Herdr) agentKind() string {
	if h.AgentKind == "" {
		return "pi"
	}
	return h.AgentKind
}

func (h *Herdr) readyTimeout() time.Duration {
	if h.ReadyTimeout <= 0 {
		return 60 * time.Second
	}
	return h.ReadyTimeout
}
