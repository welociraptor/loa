// Package dash is the one tab: what is running, and the keys to do something
// about it. It adds no policy — every action is a call that already exists —
// and it never alerts, because Herdr already marks an agent that needs input.
package dash

import (
	"context"
	"fmt"
	"strings"
	"time"

	"loa/internal/herdr"
	"loa/internal/live"
	"loa/internal/store"
)

// Msg is one event the model can handle. It is a plain value; the loop in run.go
// is what turns input and results into one.
type Msg any

// Cmd is work the loop runs off the render path. Its result comes back as a Msg.
type Cmd func() Msg

// KeyType is the kind of a decoded key press.
type KeyType int

// The key types the model understands.
const (
	KeyRunes KeyType = iota
	KeyEnter
	KeyEsc
	KeyBackspace
	KeyCtrlC
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
)

// Key is one key press, decoded from the terminal's bytes.
type Key struct {
	Type  KeyType
	Runes []rune
}

// String names the key the way the model's switch reads it.
func (k Key) String() string {
	switch k.Type {
	case KeyEnter:
		return "enter"
	case KeyEsc:
		return "esc"
	case KeyBackspace:
		return "backspace"
	case KeyCtrlC:
		return "ctrl+c"
	case KeyUp:
		return "up"
	case KeyDown:
		return "down"
	case KeyLeft:
		return "left"
	case KeyRight:
		return "right"
	default:
		return string(k.Runes)
	}
}

// Controls is everything the tab can do. The composition root supplies it, so
// the dashboard has no opinion about sessions or runners beyond these calls.
// Stop is optional so a caller that only reads and acts through the store still
// satisfies the interface; the tab reports a missing stopper rather than
// pretending it stopped anything.
type Controls interface {
	Focus(ctx context.Context, row live.Row) error
	Interrupt(ctx context.Context, row live.Row) error
	RemoveWorkspace(ctx context.Context, row live.Row) error
	Unblock(ctx context.Context, taskID int64) error
	Resolve(ctx context.Context, taskID int64, done bool, result string) error
	Run(ctx context.Context) error
}

type stopper interface {
	Stop(ctx context.Context) error
}

// Session is the read half of Herdr, as `live` needs it.
type Session = live.Session

// refreshEvery is how often the queue is re-read while the tab is open.
const refreshEvery = time.Second

// WindowSizeMsg is the terminal's current size. The loop reads it once when it
// starts and again on every SIGWINCH.
type WindowSizeMsg struct {
	Width  int
	Height int
}

// SnapshotMsg is a completed read of the queue and the active run. The refresh
// command produces it; a caller may also send one directly, which is how the
// view is tested without a terminal.
type SnapshotMsg struct {
	Snap   live.Snapshot
	Active *store.Run
	Err    error
}

type (
	tickMsg   time.Time
	actionMsg struct {
		text string
		err  error
	}
	runDoneMsg struct{ err error }
	quitMsg    struct{}
)

// prompt is the one-line input used by done/fail.
type prompt struct {
	active bool
	done   bool
	buffer string
}

// Model is the tab. It holds no session state beyond the last read, so it can
// be thrown away and rebuilt at any time.
type Model struct {
	store    *store.Store
	session  Session
	controls Controls

	rows    []live.Row
	orphans []live.Orphan
	cursor  int
	width   int
	height  int

	unavailable string
	message     string
	prompt      prompt

	// active is the run the store reports, whoever started it. The tab's own
	// run is a separate fact: runPending is set while this tab's request is in
	// flight, so a second s does not ask for another run before a refresh sees
	// the first.
	active     *store.Run
	runPending bool

	// queueMismatch explains why the database this tab draws is not the one the
	// daemon works. While it is set, s and S refuse rather than drive a queue
	// nobody is looking at.
	queueMismatch string
}

// New builds the tab. queueMismatch is the composition root's explanation of a
// database disagreement, empty when the two agree or no daemon answered.
func New(s *store.Store, session Session, controls Controls, queueMismatch string) Model {
	return Model{
		store: s, session: session, controls: controls, queueMismatch: queueMismatch,
		message: queueMismatch, width: 80, height: 24,
	}
}

// refresh re-reads the queue, the session and the active run. It runs as a
// command, never in Update, so the view never blocks on a store query.
func (m Model) refresh() Cmd {
	s, session := m.store, m.session
	return func() Msg {
		ctx := context.Background()
		snap, err := live.Read(ctx, s, session)
		if err != nil {
			return SnapshotMsg{Err: err}
		}
		active, err := s.ActiveRun(ctx)
		if err != nil {
			return SnapshotMsg{Err: err}
		}
		return SnapshotMsg{Snap: snap, Active: active}
	}
}

// act runs one control call as a command.
func (m Model) act(text string, fn func(ctx context.Context) error) Cmd {
	return func() Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return actionMsg{text: text, err: fn(ctx)}
	}
}

// Update handles input, ticks and results. Everything that touches the store or
// the session is returned as a command.
func (m Model) Update(msg Msg) (Model, Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, m.refresh()

	case WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case SnapshotMsg:
		if msg.Err != nil {
			m.message = "refresh: " + msg.Err.Error()
			return m, nil
		}
		m.rows, m.orphans, m.unavailable = msg.Snap.Rows, msg.Snap.Orphans, msg.Snap.Unavailable
		m.active = msg.Active
		if m.cursor >= len(m.rows) {
			m.cursor = max(0, len(m.rows)-1)
		}
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.message = msg.text + ": " + msg.err.Error()
		} else {
			m.message = msg.text + ": ok"
		}
		return m, m.refresh()

	case runDoneMsg:
		m.runPending = false
		if msg.err != nil {
			m.message = "run: " + msg.err.Error()
		} else {
			m.message = "run finished"
		}
		return m, m.refresh()

	case Key:
		if m.prompt.active {
			return m.updatePrompt(msg)
		}
		return m.updateKeys(msg)
	}
	return m, nil
}

func (m Model) updatePrompt(msg Key) (Model, Cmd) {
	switch msg.Type {
	case KeyEsc:
		m.prompt = prompt{}
		m.message = "cancelled"
		return m, nil
	case KeyEnter:
		p := m.prompt
		m.prompt = prompt{}
		row, ok := m.selected()
		if !ok {
			return m, nil
		}
		kind, id, result := "done", row.Task.ID, p.buffer
		if !p.done {
			kind = "fail"
		}
		return m, m.act(kind+" #"+fmt.Sprint(id), func(ctx context.Context) error {
			return m.controls.Resolve(ctx, id, p.done, result)
		})
	case KeyBackspace:
		if n := len(m.prompt.buffer); n > 0 {
			m.prompt.buffer = m.prompt.buffer[:n-1]
		}
		return m, nil
	default:
		if msg.Type == KeyRunes {
			m.prompt.buffer += string(msg.Runes)
		}
		return m, nil
	}
}

func (m Model) updateKeys(msg Key) (Model, Cmd) {
	row, hasRow := m.selected()

	switch msg.String() {
	case "q", "ctrl+c":
		// The run belongs to the daemon. Closing the tab does not stop it.
		return m, func() Msg { return quitMsg{} }
	case "j", "down":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g":
		m.cursor = 0
	case "G":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
		}

	case "enter":
		if hasRow {
			r := row
			return m, m.act("focus", func(ctx context.Context) error { return m.controls.Focus(ctx, r) })
		}
	case "x":
		if hasRow && row.AgentName != "" {
			r := row
			return m, m.act("interrupt", func(ctx context.Context) error { return m.controls.Interrupt(ctx, r) })
		}
	case "c":
		if hasRow && row.WorkspaceID != "" {
			r := row
			return m, m.act("close", func(ctx context.Context) error { return m.controls.RemoveWorkspace(ctx, r) })
		}
	case "u":
		if hasRow && row.Task.Status == store.Blocked {
			id := row.Task.ID
			return m, m.act("unblock #"+fmt.Sprint(id), func(ctx context.Context) error {
				return m.controls.Unblock(ctx, id)
			})
		}
	case "d", "f":
		if hasRow && row.Task.Status != store.Leased {
			m.prompt = prompt{active: true, done: msg.String() == "d"}
		}
	case "s":
		if m.queueMismatch != "" {
			m.message = m.queueMismatch
			return m, nil
		}
		// The store's StartRun is the guard; this is the explanation. The
		// pending request also counts, since the next snapshot may not have
		// caught the run this tab just started.
		if m.active != nil {
			m.message = fmt.Sprintf("run %d is active (owner %s)", m.active.ID, m.active.Owner)
			return m, nil
		}
		if m.runPending {
			m.message = "a run is already in flight"
			return m, nil
		}
		m.runPending = true
		m.message = "run started"
		// The daemon owns the run, so the request gets its own context: quitting
		// the tab must not cancel work the daemon is supervising.
		return m, func() Msg {
			err := m.controls.Run(context.Background())
			return runDoneMsg{err: err}
		}
	case "S":
		if m.queueMismatch != "" {
			m.message = m.queueMismatch
			return m, nil
		}
		if m.active == nil && !m.runPending {
			m.message = "no run to stop"
			return m, nil
		}
		stop, ok := m.controls.(stopper)
		if !ok {
			m.message = "stop is not available"
			return m, nil
		}
		m.message = "stopping the run"
		return m, m.act("stop", func(ctx context.Context) error { return stop.Stop(ctx) })
	}
	return m, nil
}

func (m Model) selected() (live.Row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return live.Row{}, false
	}
	return m.rows[m.cursor], true
}

// View draws the tab: what is running, then the rest, then what needs a word.
func (m Model) View() string {
	var b strings.Builder

	leased, blocked, fresh := 0, 0, 0
	for _, row := range m.rows {
		switch row.Task.Status {
		case store.Leased:
			leased++
		case store.Blocked:
			blocked++
		case store.New:
			fresh++
		}
	}
	fmt.Fprintf(&b, "loa  %d running  %d blocked  %d queued", leased, blocked, fresh)
	if m.active != nil {
		fmt.Fprintf(&b, "  [run %d %s]", m.active.ID, m.active.Owner)
	}
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "  %s %s %s %s %s\n", pad("ID", 4), pad("STATUS", 8), pad("WORKSPACE", 12), pad("SESSION", 18), "TITLE")

	// One line per task, windowed around the cursor.
	visible := max(1, m.height-6)
	first := 0
	if m.cursor >= visible {
		first = m.cursor - visible + 1
	}
	last := min(len(m.rows), first+visible)

	for i := first; i < last; i++ {
		row := m.rows[i]
		marker := "  "
		if i == m.cursor {
			marker = "> "
		}
		fmt.Fprintf(&b, "%s%s %s %s %s %s\n", marker,
			pad(fmt.Sprint(row.Task.ID), 4),
			pad(string(row.Task.Status), 8),
			pad(row.Task.Workspace, 12),
			pad(row.Session(), 18),
			truncate(row.Task.Title, max(10, m.width-45)))
	}

	for _, o := range m.orphans {
		fmt.Fprintf(&b, "  %s %s %s %s %s\n", pad("-", 4), pad("orphan", 8),
			pad(herdr.TaskLabelNumber(o.Label), 10), pad(o.WorkspaceID, 18), "(no task)")
	}

	b.WriteString("\n")
	if m.prompt.active {
		kind := "done"
		if !m.prompt.done {
			kind = "fail"
		}
		fmt.Fprintf(&b, "%s #%d result> %s", kind, m.selectedID(), m.prompt.buffer)
		return b.String()
	}

	if m.unavailable != "" {
		fmt.Fprintf(&b, "session: %s\n", m.unavailable)
	}
	if m.message != "" {
		b.WriteString(m.message + "\n")
	}
	b.WriteString("enter focus  x interrupt  d/f resolve  u unblock  c close  s/S run  q quit")
	return b.String()
}

func (m Model) selectedID() int64 {
	row, ok := m.selected()
	if !ok {
		return 0
	}
	return row.Task.ID
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:max(0, n-1)] + "…"
}
