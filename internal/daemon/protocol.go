// Package daemon is the control plane for a loa supervisor: an NDJSON protocol
// over a Unix socket, the client that speaks it, and the server that answers.
package daemon

import (
	"time"

	"loa/internal/runner"
	"loa/internal/store"
)

// Protocol is the version of the line protocol this package speaks. A request
// from another version is refused rather than guessed at.
const Protocol = 1

// Request is one line from a client. Every request names its protocol version
// so an old daemon can refuse a newer client before it touches anything.
type Request struct {
	V  int    `json:"v"`
	Do string `json:"do"`
	// Run carries this request's overrides. Zero values take the daemon's
	// configured default; a request can narrow a budget, never the policy that
	// decides whether a run may start.
	Run *RunOptions `json:"run,omitempty"`
}

// RunOptions is one run's budget, as a client may override it. The daemon's
// flags are the defaults, so a client that sets nothing runs the configured
// policy. Durations are nanoseconds on the wire, like flag.Duration values.
type RunOptions struct {
	Workers     int           `json:"workers,omitempty"`
	MaxTasks    int           `json:"max_tasks,omitempty"`
	Base        string        `json:"base,omitempty"`
	PromptsDir  string        `json:"prompts,omitempty"`
	Check       string        `json:"check,omitempty"`
	TaskTimeout time.Duration `json:"timeout,omitempty"`
	LeaseTTL    time.Duration `json:"ttl,omitempty"`
	Model       string        `json:"model,omitempty"`
	Kind        string        `json:"kind,omitempty"`
}

// Apply overlays the request's non-zero values on a daemon default. A request
// may change the budget for its run; it cannot change where the runner stores
// its work or which store it uses.
func (o *RunOptions) Apply(cfg runner.Config) runner.Config {
	if o == nil {
		return cfg
	}
	if o.Workers > 0 {
		cfg.Workers = o.Workers
	}
	if o.MaxTasks > 0 {
		cfg.MaxTasks = o.MaxTasks
	}
	if o.Base != "" {
		cfg.Base = o.Base
	}
	if o.PromptsDir != "" {
		cfg.PromptsDir = o.PromptsDir
	}
	if o.Check != "" {
		cfg.Check = o.Check
	}
	if o.TaskTimeout > 0 {
		cfg.TaskTimeout = o.TaskTimeout
	}
	if o.LeaseTTL > 0 {
		cfg.LeaseTTL = o.LeaseTTL
	}
	if h, ok := cfg.Backend.(*runner.Herdr); ok && (o.Model != "" || o.Kind != "") {
		clone := *h
		if o.Kind != "" {
			clone.AgentKind = o.Kind
		}
		if o.Model != "" {
			clone.AgentArgs = withModel(clone.AgentArgs, o.Model)
		}
		cfg.Backend = &clone
	}
	return cfg
}

// withModel replaces the agent's --model argument, or adds it. It mirrors the
// way the CLI built the same argument list before runs moved into the daemon.
func withModel(args []string, model string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--model" {
			out[i+1] = model
			return out
		}
	}
	return append(out, "--model", model)
}

// Response is one line back. V and OK are on every response, including the
// refusal of a version this daemon does not speak.
type Response struct {
	V      int        `json:"v"`
	OK     bool       `json:"ok"`
	Error  string     `json:"error,omitempty"`
	Pong   bool       `json:"pong,omitempty"`
	Status *Status    `json:"status,omitempty"`
	Run    *RunResult `json:"run,omitempty"`
}

// Status is what the daemon knows about itself.
type Status struct {
	PID      int        `json:"pid"`
	Uptime   string     `json:"uptime"`
	Protocol int        `json:"protocol"`
	Paused   bool       `json:"paused"`
	Run      *ActiveRun `json:"run,omitempty"`
}

// ActiveRun is the active run's identity, enough to tell two daemons apart in a
// status line. The full record stays in the database.
type ActiveRun struct {
	ID        int64  `json:"id"`
	Owner     string `json:"owner,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// RunRecord is the run row as it crossed the socket.
type RunRecord struct {
	ID        int64  `json:"id"`
	Owner     string `json:"owner"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	Workers   int    `json:"workers"`
	MaxTasks  int    `json:"max_tasks"`
	Done      int    `json:"done"`
	Failed    int    `json:"failed"`
}

// TaskRecord is one task's outcome as it crossed the socket. The ids and branch
// come from the row, which is what the dashboard and the next run will see.
type TaskRecord struct {
	ID          int64  `json:"id"`
	Done        bool   `json:"done"`
	Blocked     bool   `json:"blocked"`
	Branch      string `json:"branch"`
	WorkspaceID string `json:"workspace_id"`
	PaneID      string `json:"pane_id"`
	Verdict     string `json:"verdict,omitempty"`
	CheckPassed bool   `json:"check_passed"`
}

// RunResult is a finished run's record and the tasks it settled.
type RunResult struct {
	Run   RunRecord    `json:"run"`
	Tasks []TaskRecord `json:"tasks"`
}

// Stats turns the wire record back into the runner's own shape, so the CLI
// prints exactly what it printed before runs moved.
func (r *RunResult) Stats() runner.Stats {
	stats := runner.Stats{
		Run: store.Run{
			ID: r.Run.ID, Owner: r.Run.Owner, StartedAt: r.Run.StartedAt, EndedAt: r.Run.EndedAt,
			Workers: r.Run.Workers, MaxTasks: r.Run.MaxTasks, Done: r.Run.Done, Failed: r.Run.Failed,
		},
		Tasks:    len(r.Tasks),
		Outcomes: make([]runner.TaskOutcome, 0, len(r.Tasks)),
	}
	for _, task := range r.Tasks {
		if task.Done {
			stats.Completed++
		} else {
			stats.Failed++
		}
		stats.Outcomes = append(stats.Outcomes, runner.TaskOutcome{
			ID: task.ID, Done: task.Done, Blocked: task.Blocked, Branch: task.Branch,
			WorkspaceID: task.WorkspaceID, PaneID: task.PaneID,
			Verdict: task.Verdict, CheckPassed: task.CheckPassed,
		})
	}
	return stats
}

// resultOf turns a runner's result into the wire record.
func resultOf(stats runner.Stats) *RunResult {
	out := &RunResult{
		Run: RunRecord{
			ID: stats.Run.ID, Owner: stats.Run.Owner, StartedAt: stats.Run.StartedAt,
			EndedAt: stats.Run.EndedAt, Workers: stats.Run.Workers, MaxTasks: stats.Run.MaxTasks,
			Done: stats.Run.Done, Failed: stats.Run.Failed,
		},
		Tasks: make([]TaskRecord, 0, len(stats.Outcomes)),
	}
	for _, task := range stats.Outcomes {
		out.Tasks = append(out.Tasks, TaskRecord{
			ID: task.ID, Done: task.Done, Blocked: task.Blocked, Branch: task.Branch,
			WorkspaceID: task.WorkspaceID, PaneID: task.PaneID,
			Verdict: task.Verdict, CheckPassed: task.CheckPassed,
		})
	}
	return out
}
