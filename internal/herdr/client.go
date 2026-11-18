// Package herdr talks to a running Herdr server through its CLI. The CLI is the
// only interface; its JSON is the contract, and every call uses an argument
// slice so nothing is ever handed to a shell.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Error is a failure the server reported, with the machine-readable code the
// caller switches on ("agent_blocked", "agent_not_found", ...).
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`

	command string
}

func (e *Error) Error() string {
	return fmt.Sprintf("herdr %s: %s: %s", e.command, e.Code, e.Message)
}

// CodeOf returns the server's error code for err, or "" if it was not a Herdr
// error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// InSession reports whether this process is running inside a Herdr pane. The
// runner refuses to control a session it does not belong to.
func InSession() bool { return os.Getenv("HERDR_ENV") == "1" }

// Client is a thin wrapper over the herdr binary.
type Client struct {
	Bin string // default "herdr"
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "herdr"
	}
	return c.Bin
}

// exec runs one CLI call and returns its stdout. A server-reported failure
// becomes an *Error carrying the code, even though the CLI exits non-zero.
func (c *Client) exec(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	// Killing only the CLI is enough: it is a client, the server owns the work.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// A server-reported failure is more useful than the exit status.
		var envelope struct {
			Error *Error `json:"error"`
		}
		if json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &envelope) == nil && envelope.Error != nil {
			envelope.Error.command = strings.Join(args, " ")
			return nil, envelope.Error
		}
		return nil, fmt.Errorf("herdr %s: %w: %s", strings.Join(args, " "), err, tail(stderr.String(), 500))
	}
	return stdout.Bytes(), nil
}

// call runs one call and decodes its result document.
func (c *Client) call(ctx context.Context, out any, args ...string) error {
	raw, err := c.exec(ctx, args...)
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("herdr %s: decode: %w", strings.Join(args, " "), err)
	}
	if envelope.Error != nil {
		envelope.Error.command = strings.Join(args, " ")
		return envelope.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("herdr %s: decode result: %w", strings.Join(args, " "), err)
	}
	return nil
}

func tail(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= n {
		return string(runes)
	}
	return "…" + string(runes[len(runes)-n:])
}

// Status is the client/server handshake the runner checks before it starts.
type Status struct {
	Client struct {
		Version string `json:"version"`
	} `json:"client"`
	Server struct {
		Running    bool   `json:"running"`
		Version    string `json:"version"`
		Protocol   int    `json:"protocol"`
		Compatible bool   `json:"compatible"`
	} `json:"server"`
}

// CheckStatus verifies that a compatible server is running.
func (c *Client) CheckStatus(ctx context.Context) (Status, error) {
	// Unlike the mutating calls, the status document is not wrapped in "result".
	raw, err := c.exec(ctx, "status", "--json")
	if err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("herdr status: decode: %w", err)
	}
	if !s.Server.Running {
		return s, errors.New("herdr: no server is running")
	}
	if !s.Server.Compatible {
		return s, fmt.Errorf("herdr: client %s is not compatible with server %s (protocol %d)",
			s.Client.Version, s.Server.Version, s.Server.Protocol)
	}
	return s, nil
}

// Workspace is one Herdr workspace. The label is what a task refers to; the id
// is a handle that changes meaning when a workspace is recreated.
type Workspace struct {
	ID          string `json:"workspace_id"`
	Label       string `json:"label"`
	AgentStatus string `json:"agent_status"`
	PaneCount   int    `json:"pane_count"`
	TabCount    int    `json:"tab_count"`
}

// Workspaces lists every workspace in the session.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	var out struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := c.call(ctx, &out, "workspace", "list"); err != nil {
		return nil, err
	}
	return out.Workspaces, nil
}

// Agent is a recognised agent living in a pane.
type Agent struct {
	Name        string `json:"name"`
	Kind        string `json:"agent"`
	Status      string `json:"agent_status"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Cwd         string `json:"cwd"`
}

// Agents lists every recognised agent in the session.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var out struct {
		Agents []Agent `json:"agents"`
	}
	if err := c.call(ctx, &out, "agent", "list"); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// WorktreeRequest is one task's worktree. The workspace picks the repository
// (Herdr rejects --workspace together with --cwd) and Herdr's own configuration
// picks the directory; the created path comes back in the result.
type WorktreeRequest struct {
	WorkspaceID string
	Branch      string
	Base        string
	Label       string
}

// Worktree is what Herdr created: the workspace and root pane the agent will
// occupy, and the checkout.
type Worktree struct {
	WorkspaceID string
	TabID       string
	PaneID      string
	Path        string
	Repo        string
	Branch      string
}

// CreateWorktree asks Herdr for a linked worktree and the workspace around it.
// --no-focus keeps the human's attention where it was.
func (c *Client) CreateWorktree(ctx context.Context, req WorktreeRequest) (Worktree, error) {
	if req.WorkspaceID == "" {
		return Worktree{}, errors.New("herdr: worktree needs a workspace id")
	}
	args := []string{
		"worktree", "create",
		"--workspace", req.WorkspaceID,
		"--branch", req.Branch,
		"--base", req.Base,
		"--label", req.Label,
		"--no-focus",
	}

	var out struct {
		Workspace struct {
			ID       string `json:"workspace_id"`
			Worktree struct {
				RepoRoot string `json:"repo_root"`
			} `json:"worktree"`
		} `json:"workspace"`
		Tab struct {
			ID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
		Worktree struct {
			Path   string `json:"path"`
			Branch string `json:"branch"`
		} `json:"worktree"`
	}
	if err := c.call(ctx, &out, args...); err != nil {
		return Worktree{}, err
	}

	created := Worktree{
		WorkspaceID: out.Workspace.ID,
		TabID:       out.Tab.ID,
		PaneID:      out.RootPane.ID,
		Path:        out.Worktree.Path,
		Repo:        out.Workspace.Worktree.RepoRoot,
		Branch:      out.Worktree.Branch,
	}
	if created.WorkspaceID == "" || created.PaneID == "" {
		return created, errors.New("herdr: worktree create returned no workspace or pane")
	}
	return created, nil
}

// RemoveWorktree deletes the checkout and closes the workspace around it.
func (c *Client) RemoveWorktree(ctx context.Context, workspaceID string) error {
	return c.call(ctx, nil, "worktree", "remove", "--workspace", workspaceID, "--force")
}

// CloseWorkspace closes one workspace, never its group: the worktree workspace
// is linked to the repository's, and closing the group would take that too.
func (c *Client) CloseWorkspace(ctx context.Context, workspaceID string) error {
	return c.call(ctx, nil, "workspace", "close", workspaceID)
}

// StartAgentRequest is one agent in an existing pane.
type StartAgentRequest struct {
	Name   string // [a-z][a-z0-9_-]{0,31}, unique among live agents
	Kind   string // "pi", "claude", ...
	PaneID string
	Args   []string // native agent arguments, after "--"
	// Timeout bounds how long Herdr waits for the agent to become ready.
	Timeout time.Duration
}

// StartAgent starts an agent and returns once Herdr considers it ready.
func (c *Client) StartAgent(ctx context.Context, req StartAgentRequest) error {
	args := []string{"agent", "start", req.Name, "--kind", req.Kind, "--pane", req.PaneID}
	if req.Timeout > 0 {
		args = append(args, "--timeout", fmt.Sprint(req.Timeout.Milliseconds()))
	}
	if len(req.Args) > 0 {
		args = append(args, "--")
		args = append(args, req.Args...)
	}
	return c.call(ctx, nil, args...)
}

// Prompt submits text to an agent and waits until it settles. A blocked agent is
// rejected before any input is written, as an *Error with code agent_blocked.
func (c *Client) Prompt(ctx context.Context, target, text string, timeout time.Duration) error {
	args := []string{"agent", "prompt", target, text, "--wait"}
	if timeout > 0 {
		args = append(args, "--timeout", fmt.Sprint(timeout.Milliseconds()))
	}
	return c.call(ctx, nil, args...)
}

// Read returns the agent's recent terminal output as text.
func (c *Client) Read(ctx context.Context, target string, lines int) (string, error) {
	raw, err := c.exec(ctx, "agent", "read", target, "--source", "recent-unwrapped",
		"--lines", fmt.Sprint(lines), "--format", "text")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// SendKeys sends logical key presses, e.g. "ctrl+c" or "esc".
func (c *Client) SendKeys(ctx context.Context, target string, keys ...string) error {
	if len(keys) == 0 {
		return errors.New("herdr: no keys to send")
	}
	return c.call(ctx, nil, append([]string{"agent", "send-keys", target}, keys...)...)
}

// Wait blocks until the agent reports one of the given states.
func (c *Client) Wait(ctx context.Context, target string, timeout time.Duration, states ...string) error {
	args := []string{"agent", "wait", target}
	for _, s := range states {
		args = append(args, "--until", s)
	}
	if timeout > 0 {
		args = append(args, "--timeout", fmt.Sprint(timeout.Milliseconds()))
	}
	return c.call(ctx, nil, args...)
}
