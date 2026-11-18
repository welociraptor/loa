// Package report turns a queue snapshot into the one page a person reads after
// a day away. It is deterministic and needs no model: the summary has to exist
// on the day the model does not.
package report

import (
	"fmt"
	"strings"
	"time"

	"loa/internal/live"
	"loa/internal/store"
)

// Item is one task in a section.
type Item struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Workspace string    `json:"workspace"`
	Branch    string    `json:"branch,omitempty"`
	Session   string    `json:"session,omitempty"`
	Result    string    `json:"result,omitempty"`
	RunMS     int64     `json:"run_ms,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Report is what happened in a window, and what is still lying around.
type Report struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	Done    []Item `json:"done"`
	Blocked []Item `json:"blocked"`
	Queued  []Item `json:"queued"`

	// Open is every task whose workspace still exists, window or not: that is the
	// residue a person should look at.
	Open []Item `json:"open"`
	// Orphans are task-labelled workspaces whose task is not in the queue.
	Orphans []live.Orphan `json:"orphans,omitempty"`

	// LastActivity is the newest update in the queue; Idle is how long ago it was.
	LastActivity time.Time     `json:"last_activity"`
	Idle         time.Duration `json:"idle_ns"`

	SessionUnavailable string `json:"session_unavailable,omitempty"`
}

// Build groups a snapshot into a report. Tasks are counted when they changed
// inside the window; workspaces are reported regardless of it, because a
// workspace left open yesterday is still open today.
func Build(snap live.Snapshot, since, now time.Time) Report {
	r := Report{Since: since, Until: now, SessionUnavailable: snap.Unavailable}

	for _, row := range snap.Rows {
		task := row.Task
		if task.UpdatedAt.After(r.LastActivity) {
			r.LastActivity = task.UpdatedAt
		}

		item := Item{
			ID:        task.ID,
			Title:     task.Title,
			Workspace: task.Workspace,
			Branch:    task.Branch,
			Session:   row.Session(),
			Result:    task.Result,
			RunMS:     task.RunMS,
			UpdatedAt: task.UpdatedAt,
		}

		inWindow := !task.UpdatedAt.Before(since)
		switch {
		case task.Status == store.Done && inWindow:
			r.Done = append(r.Done, item)
		case task.Status == store.Blocked && inWindow:
			r.Blocked = append(r.Blocked, item)
		case task.Status == store.New:
			r.Queued = append(r.Queued, item)
		}

		if row.WorkspaceState == live.WorkspaceLive || row.WorkspaceState == live.WorkspaceUntracked {
			r.Open = append(r.Open, item)
		}
	}

	if !r.LastActivity.IsZero() {
		r.Idle = now.Sub(r.LastActivity)
	}
	r.Orphans = snap.Orphans
	return r
}

// Text renders the page. Sections that are empty are omitted, but the gap is
// always stated: a silent report and an idle queue must not look the same.
func (r Report) Text() string {
	var b strings.Builder

	fmt.Fprintf(&b, "since %s, %s ago\n\n",
		r.Since.UTC().Format(time.RFC3339), r.Until.Sub(r.Since).Round(time.Minute))

	section(&b, "done", r.Done, func(i Item) string {
		return fmt.Sprintf("#%d  %s   %s   %s   %s",
			i.ID, i.Title, branch(i.Branch), ran(i.RunMS), oneLine(i.Result))
	})
	section(&b, "needs a human", r.Blocked, func(i Item) string {
		return fmt.Sprintf("#%d  %s   %s", i.ID, i.Title, oneLine(i.Result))
	})
	section(&b, "queued", r.Queued, func(i Item) string {
		return fmt.Sprintf("#%d  %s", i.ID, i.Title)
	})
	section(&b, "still open", r.Open, func(i Item) string {
		return fmt.Sprintf("#%d  %s   %s", i.ID, i.Title, i.Session)
	})
	for _, o := range r.Orphans {
		fmt.Fprintf(&b, "orphaned workspace %s (%s): no task accounts for it\n", o.WorkspaceID, o.Label)
	}

	if total := r.TotalRun(); total > 0 {
		fmt.Fprintf(&b, "time in agents: %s\n", total.Round(time.Second))
	}
	if r.SessionUnavailable != "" {
		fmt.Fprintf(&b, "session: %s\n", r.SessionUnavailable)
	}
	b.WriteString(r.Gap())
	return b.String()
}

func section(b *strings.Builder, name string, items []Item, line func(Item) string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s: %d\n", name, len(items))
	for _, item := range items {
		fmt.Fprintf(b, "  %s\n", line(item))
	}
	b.WriteString("\n")
}

// Gap states how long the queue has been still. It is the section that makes an
// unsupervised week visible rather than silent.
func (r Report) Gap() string {
	if r.LastActivity.IsZero() {
		return "the queue has never been worked: no task has been added\n"
	}
	if r.Idle < time.Minute {
		return "activity: just now\n"
	}
	return fmt.Sprintf("nothing has moved since %s (%s ago)\n",
		r.LastActivity.UTC().Format(time.RFC3339), r.Idle.Round(time.Minute))
}

// TotalRun is the agent time spent by the tasks in this report.
func (r Report) TotalRun() time.Duration {
	var total time.Duration
	for _, group := range [][]Item{r.Done, r.Blocked, r.Open} {
		for _, item := range group {
			total += time.Duration(item.RunMS) * time.Millisecond
		}
	}
	return total
}

func branch(b string) string {
	if b == "" {
		return "(no branch)"
	}
	return b
}

// ran reports the attempt's wall time. Zero means it was never measured, which
// is what a task resolved by hand looks like.
func ran(ms int64) string {
	if ms <= 0 {
		return "ran ?"
	}
	return "ran " + (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

// oneLine keeps a failure reason to a line, since it is often a screen of text.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 100 {
		return s
	}
	return s[:99] + "…"
}
