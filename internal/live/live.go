// Package live joins the queue with the state of the session. The store is
// authoritative: the session only says what is happening to a task right now,
// and a row is still worth showing when the session cannot be read at all.
package live

import (
	"context"
	"fmt"
	"sort"

	"taskflow/internal/herdr"
	"taskflow/internal/store"
)

// Session is the part of Herdr this package needs. Recorded JSON fixtures pin
// the shapes; a fake covers the joining.
type Session interface {
	Workspaces(ctx context.Context) ([]herdr.Workspace, error)
	Agents(ctx context.Context) ([]herdr.Agent, error)
}

// How a task's workspace stands right now.
const (
	WorkspaceLive      = "live"      // the workspace exists
	WorkspaceClosed    = "closed"    // recorded, but the workspace is gone
	WorkspaceNone      = "none"      // nothing was ever recorded or created
	WorkspaceUntracked = "untracked" // live, found by label only; not recorded yet
)

// Row is one task with whatever the session knows about it.
type Row struct {
	Task store.Task

	// WorkspaceID is the workspace this attempt owns: the recorded id, or the
	// one carrying the task's label when nothing was recorded.
	WorkspaceID    string
	WorkspaceState string

	// AgentName and AgentState come from the agent in that workspace, if any.
	// They are decoration: a workspace outlives the agent inside it.
	AgentName  string
	AgentState string
}

// Orphan is a task-labelled workspace that nothing in the queue accounts for:
// its task was deleted, or the label names a task that never existed. A
// workspace labelled for a task that is merely undispatched is that task's row,
// not an orphan.
type Orphan struct {
	TaskID      int64
	WorkspaceID string
	Label       string
}

// Snapshot is everything `ps` shows in one read.
type Snapshot struct {
	Rows    []Row
	Orphans []Orphan

	// Unavailable is set when the session could not be read. The store half is
	// still valid, so this degrades the view instead of failing it.
	Unavailable string
}

// Read joins the queue with the session. Store errors are returned; session
// errors only mark the snapshot as degraded.
func Read(ctx context.Context, s *store.Store, session Session) (Snapshot, error) {
	tasks, err := s.List(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	claims, err := s.Claims(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	claimedBy := make(map[string]int64, len(claims))
	for _, c := range claims {
		claimedBy[c.WorkspaceID] = c.TaskID
	}

	var snap Snapshot
	workspaces, agents, unavailable := sessionState(ctx, session)
	snap.Unavailable = unavailable

	byID := make(map[string]herdr.Workspace, len(workspaces))
	byLabel := make(map[int64]herdr.Workspace, len(workspaces))
	for _, w := range workspaces {
		byID[w.ID] = w
		if id, ok := herdr.ParseTaskLabel(w.Label); ok {
			byLabel[id] = w
		}
	}
	agentByWorkspace := make(map[string]herdr.Agent, len(agents))
	for _, a := range agents {
		agentByWorkspace[a.WorkspaceID] = a
	}

	matched := make(map[string]bool, len(workspaces))
	for _, task := range tasks {
		row := Row{Task: task, WorkspaceState: WorkspaceNone}

		if task.WorkspaceID != "" {
			if workspace, recorded := byID[task.WorkspaceID]; recorded {
				row.WorkspaceID, row.WorkspaceState = workspace.ID, WorkspaceLive
				matched[workspace.ID] = true
			} else {
				// Recorded, but the workspace is gone: the attempt ran and was
				// cleaned up, or somebody closed it by hand.
				row.WorkspaceID, row.WorkspaceState = task.WorkspaceID, WorkspaceClosed
			}
		} else if unavailable == "" {
			// Nothing recorded. The label is the only evidence, which is also the
			// window reconciliation treats as unattributed.
			if workspace, spotted := byLabel[task.ID]; spotted {
				row.WorkspaceID, row.WorkspaceState = workspace.ID, WorkspaceUntracked
				matched[workspace.ID] = true
			}
		}

		if agent, ok := agentByWorkspace[row.WorkspaceID]; ok && row.WorkspaceState != WorkspaceNone {
			row.AgentName, row.AgentState = agent.Name, agent.Status
		}
		snap.Rows = append(snap.Rows, row)
	}

	if unavailable == "" {
		for _, workspace := range workspaces {
			taskID, ok := herdr.ParseTaskLabel(workspace.Label)
			if !ok || matched[workspace.ID] {
				continue
			}
			if _, claimed := claimedBy[workspace.ID]; claimed {
				continue
			}
			snap.Orphans = append(snap.Orphans, Orphan{
				TaskID:      taskID,
				WorkspaceID: workspace.ID,
				Label:       workspace.Label,
			})
		}
	}

	Sort(snap.Rows)
	return snap, nil
}

// sessionState reads the session, reporting a reason instead of an error.
func sessionState(ctx context.Context, session Session) ([]herdr.Workspace, []herdr.Agent, string) {
	workspaces, err := session.Workspaces(ctx)
	if err != nil {
		return nil, nil, fmt.Sprintf("session unavailable: %v", err)
	}
	agents, err := session.Agents(ctx)
	if err != nil {
		return workspaces, nil, fmt.Sprintf("agents unavailable: %v", err)
	}
	return workspaces, agents, ""
}

// Sort puts what is live first: leased, blocked, new by priority, done last.
func Sort(rows []Row) {
	rank := func(status store.Status) int {
		switch status {
		case store.Leased:
			return 0
		case store.Blocked:
			return 1
		case store.New:
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := rank(rows[i].Task.Status), rank(rows[j].Task.Status)
		if ri != rj {
			return ri < rj
		}
		if rows[i].Task.Priority != rows[j].Task.Priority {
			return rows[i].Task.Priority > rows[j].Task.Priority
		}
		return rows[i].Task.ID < rows[j].Task.ID
	})
}

// Session is what a table cell says about a row's session.
func (r Row) Session() string {
	switch r.WorkspaceState {
	case WorkspaceNone:
		return "-"
	case WorkspaceClosed:
		return "closed"
	}
	if r.AgentName == "" && r.AgentState == "" {
		return r.WorkspaceID + " no agent"
	}
	if r.AgentName == "" {
		return r.WorkspaceID + " " + r.AgentState
	}
	return r.WorkspaceID + " " + r.AgentState + " " + r.AgentName
}
