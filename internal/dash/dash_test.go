package dash_test

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"taskflow/internal/dash"
	"taskflow/internal/herdr"
	"taskflow/internal/live"
	"taskflow/internal/store"
)

type fakeSession struct {
	workspaces []herdr.Workspace
	agents     []herdr.Agent
}

func (f fakeSession) Workspaces(context.Context) ([]herdr.Workspace, error) {
	return f.workspaces, nil
}

func (f fakeSession) Agents(context.Context) ([]herdr.Agent, error) { return f.agents, nil }

// fakeControls records what the tab asked for.
type fakeControls struct {
	calls   []string
	err     error
	runHook func(ctx context.Context) error
}

func (f *fakeControls) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeControls) Focus(_ context.Context, row live.Row) error {
	f.record("focus " + row.WorkspaceID)
	return f.err
}

func (f *fakeControls) Interrupt(_ context.Context, row live.Row) error {
	f.record("interrupt " + row.AgentName)
	return f.err
}

func (f *fakeControls) RemoveWorkspace(_ context.Context, row live.Row) error {
	f.record("close " + row.WorkspaceID)
	return f.err
}

func (f *fakeControls) Unblock(_ context.Context, id int64) error {
	f.record("unblock " + strconv.FormatInt(id, 10))
	return f.err
}

func (f *fakeControls) Resolve(_ context.Context, id int64, done bool, result string) error {
	verb := "fail "
	if done {
		verb = "done "
	}
	f.record(verb + strconv.FormatInt(id, 10) + " " + result)
	return f.err
}

func (f *fakeControls) Run(ctx context.Context) error {
	f.record("run")
	if f.runHook != nil {
		return f.runHook(ctx)
	}
	return f.err
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seed builds a queue with one leased task (whose agent is blocked), one blocked
// task and one queued task, plus the session state that belongs with them.
func seed(t *testing.T) (*store.Store, fakeSession, int64, int64) {
	t.Helper()
	ctx := context.Background()
	s := newStore(t)

	running, err := s.Add(ctx, store.NewTask{Title: "running work", Workspace: "nixos", Priority: 5})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	blocked, err := s.Add(ctx, store.NewTask{Title: "needs a human", Workspace: "nixos", MaxTries: 1})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.Add(ctx, store.NewTask{Title: "waiting its turn", Workspace: "nixos"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := s.Lease(ctx, "other", time.Hour, time.Now()); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := s.Attach(ctx, running, "w1", "w1:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := s.Lease(ctx, "other", time.Hour, time.Now()); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := s.Attach(ctx, blocked, "w2", "w2:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.Fail(ctx, blocked, "the agent gave up"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	return s, fakeSession{
		workspaces: []herdr.Workspace{{ID: "w1", Label: herdr.TaskLabel(running)}},
		agents:     []herdr.Agent{{Name: "tf-1-1", Status: "blocked", WorkspaceID: "w1"}},
	}, running, blocked
}

// loaded returns a model that has already read the queue.
func loaded(t *testing.T, s *store.Store, session fakeSession, c dash.Controls) dash.Model {
	t.Helper()
	m := dash.New(s, session, c)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = asModel(t, next)

	snap, err := live.Read(context.Background(), s, session)
	if err != nil {
		t.Fatalf("live read: %v", err)
	}
	next, _ = m.Update(dash.SnapshotMsg{Snap: snap})
	return asModel(t, next)
}

func key(s string) tea.KeyMsg {
	if t, ok := map[string]tea.KeyType{"enter": tea.KeyEnter, "esc": tea.KeyEsc, "backspace": tea.KeyBackspace}[s]; ok {
		return tea.KeyMsg{Type: t}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func run(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command to run")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("command produced no message")
	}
	return msg
}

// rowLines returns the rendered task lines, in order, without the header.
func rowLines(m dash.Model) []string {
	var rows []string
	for _, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(line, "WORKSPACE") {
			continue
		}
		if strings.HasPrefix(line, "> ") {
			rows = append(rows, line)
		}
	}
	return rows
}

func TestViewShowsLiveWorkFirst(t *testing.T) {
	s, session, _, _ := seed(t)
	m := loaded(t, s, session, &fakeControls{})

	view := m.View()
	if !strings.Contains(view, "1 running") || !strings.Contains(view, "1 blocked") || !strings.Contains(view, "1 queued") {
		t.Fatalf("header = %q", strings.Split(view, "\n")[0])
	}
	// The leased task is selected, and its session names the blocked agent.
	rows := rowLines(m)
	if len(rows) != 1 || !strings.Contains(rows[0], "running work") || !strings.Contains(rows[0], "w1 blocked tf-1-1") {
		t.Fatalf("selected row = %v", rows)
	}
	for _, want := range []string{"needs a human", "waiting its turn", "enter focus", "q quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q", want)
		}
	}
}

func TestSelectionMovesAndStopsAtTheEnds(t *testing.T) {
	s, session, _, _ := seed(t)
	c := &fakeControls{}
	m := loaded(t, s, session, c)

	m = step(t, m, key("j"))
	if got := cursor(m); got != 1 {
		t.Fatalf("j moved to %d, want 1", got)
	}
	// Row 1 is the blocked task, whose workspace was closed already, so focus
	// still targets the recorded workspace.
	run(t, mustCmd(t, m, key("enter")))
	if got := c.calls[0]; got != "focus w2" {
		t.Errorf("focus on the second row = %q", got)
	}

	m = step(t, m, key("G"))
	if got := cursor(m); got != 2 {
		t.Errorf("G went to %d, want the last row", got)
	}
	m = step(t, m, key("j"))
	if got := cursor(m); got != 2 {
		t.Errorf("j moved past the last row to %d", got)
	}
	m = step(t, m, key("g"))
	if got := cursor(m); got != 0 {
		t.Errorf("g went to %d, want the top", got)
	}
}

func TestXInterruptsTheAgentOfTheSelectedTask(t *testing.T) {
	s, session, _, _ := seed(t)
	c := &fakeControls{}
	m := loaded(t, s, session, c)

	run(t, mustCmd(t, m, key("x")))
	if len(c.calls) != 1 || c.calls[0] != "interrupt tf-1-1" {
		t.Fatalf("calls = %v", c.calls)
	}
}

func TestUnblockOnlyAppliesToBlockedTasks(t *testing.T) {
	s, session, _, blocked := seed(t)
	c := &fakeControls{}
	m := loaded(t, s, session, c)

	// Row 0 is leased: u is refused, silently.
	if _, cmd := m.Update(key("u")); cmd != nil {
		t.Fatalf("u on a leased task produced a command")
	}

	m = step(t, m, key("j"))
	run(t, mustCmd(t, m, key("u")))
	if len(c.calls) != 1 || c.calls[0] != "unblock "+strconv.FormatInt(blocked, 10) {
		t.Fatalf("calls = %v", c.calls)
	}
}

func TestResolvePromptTakesAResult(t *testing.T) {
	s, session, _, blocked := seed(t)
	c := &fakeControls{}
	m := loaded(t, s, session, c)
	m = step(t, m, key("j"))

	m = step(t, m, key("d"))
	if !strings.Contains(m.View(), "done #"+strconv.FormatInt(blocked, 10)+" result>") {
		t.Fatalf("prompt did not open:\n%s", m.View())
	}
	for _, r := range "approved by hand" {
		m = step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	next, cmd := m.Update(key("enter"))
	run(t, cmd)
	m = asModel(t, next)
	if len(c.calls) != 1 || c.calls[0] != "done "+strconv.FormatInt(blocked, 10)+" approved by hand" {
		t.Fatalf("calls = %v", c.calls)
	}
	if strings.Contains(m.View(), "result>") {
		t.Errorf("the prompt stayed open")
	}
}

func TestEscapeAbandonsThePrompt(t *testing.T) {
	s, session, _, _ := seed(t)
	c := &fakeControls{}
	m := loaded(t, s, session, c)
	m = step(t, m, key("j"))
	m = step(t, m, key("f"))
	m = step(t, m, key("esc"))

	// Typing after escape is a key press, not a prompt character.
	if _, cmd := m.Update(key("enter")); cmd != nil {
		_ = run(t, cmd)
	}
	for _, call := range c.calls {
		if strings.HasPrefix(call, "fail") || strings.HasPrefix(call, "done") {
			t.Fatalf("escape still resolved the task: %v", c.calls)
		}
	}
}

func TestOneRunAtATime(t *testing.T) {
	s, session, _, _ := seed(t)
	c := &fakeControls{}
	block := make(chan struct{})
	c.runHook = func(context.Context) error {
		<-block
		return nil
	}
	m := loaded(t, s, session, c)

	next, cmd := m.Update(key("s"))
	if cmd == nil {
		t.Fatal("s produced no command")
	}
	m = asModel(t, next)
	if !strings.Contains(m.View(), "[running]") {
		t.Errorf("the tab does not say a run is in flight")
	}

	next, second := m.Update(key("s"))
	if second != nil {
		t.Fatal("a second run was started")
	}
	if !strings.Contains(asModel(t, next).View(), "already in flight") {
		t.Errorf("no explanation for the refusal")
	}

	close(block)
	next, _ = m.Update(run(t, cmd))
	if strings.Contains(asModel(t, next).View(), "[running]") {
		t.Errorf("the tab still claims a run is in flight")
	}
}

func TestControlsErrorsLandInTheMessageLine(t *testing.T) {
	s, session, _, _ := seed(t)
	c := &fakeControls{err: context.DeadlineExceeded}
	m := loaded(t, s, session, c)

	msg := run(t, mustCmd(t, m, key("x")))
	next, _ := m.Update(msg)
	if !strings.Contains(asModel(t, next).View(), "interrupt: context deadline exceeded") {
		t.Errorf("the failure was not shown:\n%s", asModel(t, next).View())
	}
}

func step(t *testing.T, m dash.Model, msg tea.Msg) dash.Model {
	t.Helper()
	next, _ := m.Update(msg)
	return asModel(t, next)
}

func mustCmd(t *testing.T, m dash.Model, msg tea.Msg) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatalf("%v produced no command", msg)
	}
	return cmd
}

// asModel asserts the model type once, so tests read as flow rather than type
// switches.
func asModel(t *testing.T, m tea.Model) dash.Model {
	t.Helper()
	model, ok := m.(dash.Model)
	if !ok {
		t.Fatalf("update returned %T, want dash.Model", m)
	}
	return model
}

// cursor is the index of the row the marker is on.
func cursor(m dash.Model) int {
	index := -1
	seen := 0
	for _, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(line, "WORKSPACE") || strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "> ") {
			continue
		}
		if strings.HasPrefix(line, "> ") {
			index = seen
		}
		seen++
	}
	return index
}
