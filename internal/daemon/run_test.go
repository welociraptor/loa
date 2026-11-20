package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"loa/internal/runner"
	"loa/internal/store"
)

// blockingBackend is a task's work as something that can outlive a client: the
// agent waits until the run is stopped or the test lets it go.
type blockingBackend struct {
	started  chan struct{}
	prompted chan struct{}
	once     sync.Once
	prompt   sync.Once

	mu     sync.Mutex
	closed []string
}

func newBlockingBackend() *blockingBackend {
	return &blockingBackend{started: make(chan struct{}), prompted: make(chan struct{})}
}

func (b *blockingBackend) Open(_ context.Context, req runner.OpenRequest) (runner.Handle, error) {
	b.once.Do(func() { close(b.started) })
	return runner.Handle{
		TaskID: req.TaskID, AgentName: req.AgentName,
		WorkspaceID: "w" + itoa(req.TaskID), PaneID: "p1",
		Branch: req.Branch,
	}, nil
}

func (b *blockingBackend) Prompt(ctx context.Context, _ runner.Handle, _ string, _ time.Duration) error {
	b.prompt.Do(func() { close(b.prompted) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return nil
	}
}

func (b *blockingBackend) Read(context.Context, runner.Handle, int) (string, error) {
	return "", nil
}

func (b *blockingBackend) Interrupt(context.Context, runner.Handle) error { return nil }

func (b *blockingBackend) Close(_ context.Context, h runner.Handle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = append(b.closed, h.WorkspaceID)
	return nil
}

func (b *blockingBackend) Label(id int64) string { return "loa #" + itoa(id) }

func (b *blockingBackend) TaskWorkspaces(context.Context) ([]runner.TaskWorkspace, error) {
	return nil, nil
}

func (b *blockingBackend) Closed() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.closed...)
}

func itoa(n int64) string {
	// strconv would be another import for one call in a test helper.
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func daemonStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addRunTask(t *testing.T, s *store.Store) *store.Task {
	t.Helper()
	id, err := s.Add(context.Background(), store.NewTask{Title: "work", Workspace: "loa"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	task, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return task
}

func runConfig(s *store.Store, b runner.Backend) runner.Config {
	return runner.Config{
		Store: s, Backend: b, Workers: 2, MaxTasks: 2,
		LeaseTTL: 2 * time.Hour, TaskTimeout: 45 * time.Minute,
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunStartedByOneClientIsVisibleToAnother(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	task := addRunTask(t, s)
	r := start(t, Config{Runner: runConfig(s, b)})
	defer r.stop(t)

	client := &Client{Path: r.sock}
	result := make(chan error, 1)
	var stats runner.Stats
	go func() {
		var err error
		stats, err = client.Run(ctx, RunOptions{MaxTasks: 1})
		result <- err
	}()

	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never opened a workspace")
	}

	status, err := (&Client{Path: r.sock}).Status(ctx)
	if err != nil {
		t.Fatalf("status from the second client: %v", err)
	}
	if status.Run == nil || status.Run.ID == 0 {
		t.Fatalf("status = %+v, want the run the first client started", status)
	}

	if err := (&Client{Path: r.sock}).Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Error("a stopped run reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first client's run request did not return after stop")
	}

	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != store.Leased {
		t.Errorf("task status = %s, want leased; a stopped run leaves its lease to lapse", got.Status)
	}
	active, err := s.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active != nil {
		t.Errorf("run %d is still open", active.ID)
	}
	if stats.Run.ID == 0 || stats.Run.EndedAt == "" {
		t.Errorf("stats = %+v, want the finished run row", stats.Run)
	}
}

func TestClientDisconnectLeavesTheRunGoing(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	task := addRunTask(t, s)
	r := start(t, Config{Runner: runConfig(s, b)})
	defer r.stop(t)

	conn, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte(`{"v":1,"do":"run","run":{"max_tasks":1}}` + "\n")); err != nil {
		t.Fatalf("write run request: %v", err)
	}
	// The client is gone before its run has even opened a workspace.
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the disconnected client's run did not start")
	}

	status, err := (&Client{Path: r.sock}).Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Run == nil {
		t.Fatal("the run vanished with its client")
	}

	if err := (&Client{Path: r.sock}).Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, "the disconnected client's run to finish", func() bool {
		active, err := s.ActiveRun(ctx)
		return err == nil && active == nil
	})

	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != store.Leased {
		t.Errorf("task status = %s, want leased; a disconnect must not settle the task", got.Status)
	}
}

func TestDeadRunRowIsSettledAtTheNextStart(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	dead, err := s.StartRun(ctx, "dead-daemon/999999", 1, 1)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	r := start(t, Config{Runner: runConfig(s, nil)})
	defer r.stop(t)

	got, err := s.GetRun(ctx, dead.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Active() {
		t.Errorf("run %d is still active after the daemon started", got.ID)
	}
	status, err := (&Client{Path: r.sock}).Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Run != nil {
		t.Errorf("status reports a run after settling: %+v", status.Run)
	}
}

func TestDaemonShutdownCancelsTheRunAndLeavesTheLease(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	task := addRunTask(t, s)
	r := start(t, Config{Runner: runConfig(s, b)})

	result := make(chan error, 1)
	go func() {
		_, err := (&Client{Path: r.sock}).Run(ctx, RunOptions{MaxTasks: 1})
		result <- err
	}()

	select {
	case <-b.prompted:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never prompted its agent")
	}

	r.stop(t)

	select {
	case err := <-result:
		if err == nil {
			t.Error("a run interrupted by shutdown reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run request did not return after shutdown")
	}
	if closed := b.Closed(); len(closed) == 0 {
		t.Error("shutdown left the workspace open")
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != store.Leased {
		t.Errorf("task status = %s, want leased", got.Status)
	}
	active, err := s.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active != nil {
		t.Errorf("run %d is still open after the daemon stopped", active.ID)
	}
}

func TestRunUsesTheDaemonsBudgetUnlessTheRequestNarrowsIt(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	r := start(t, Config{Runner: runConfig(s, b)})
	defer r.stop(t)

	client := &Client{Path: r.sock}
	if _, err := client.Run(ctx, RunOptions{}); err != nil {
		t.Fatalf("default run: %v", err)
	}
	runs, err := s.Runs(ctx, 1)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Workers != 2 || runs[0].MaxTasks != 2 {
		t.Fatalf("default run = %+v, want the daemon's 2/2", runs)
	}

	if _, err := client.Run(ctx, RunOptions{Workers: 1, MaxTasks: 1}); err != nil {
		t.Fatalf("override run: %v", err)
	}
	runs, err = s.Runs(ctx, 1)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Workers != 1 || runs[0].MaxTasks != 1 {
		t.Fatalf("override run = %+v, want the request's 1/1", runs)
	}
}

func TestRunRequestCannotWidenTheDaemonsBudget(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	r := start(t, Config{Runner: runConfig(s, newBlockingBackend())})
	defer r.stop(t)

	if _, err := (&Client{Path: r.sock}).Run(ctx, RunOptions{Workers: 9, MaxTasks: 9}); err != nil {
		t.Fatalf("run: %v", err)
	}
	runs, err := s.Runs(ctx, 1)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Workers != 2 || runs[0].MaxTasks != 2 {
		t.Fatalf("run = %+v, want the daemon's 2/2", runs)
	}
}

func TestRunRequestCannotLengthenTimeoutOrTTL(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	cfg := runConfig(s, newBlockingBackend())
	r := start(t, Config{Runner: cfg})
	defer r.stop(t)
	client := &Client{Path: r.sock}

	for _, tc := range []struct {
		name string
		opts RunOptions
		want time.Duration
	}{
		{"timeout", RunOptions{TaskTimeout: cfg.TaskTimeout + time.Minute}, cfg.TaskTimeout},
		{"ttl", RunOptions{LeaseTTL: cfg.LeaseTTL + time.Minute}, cfg.LeaseTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Run(ctx, tc.opts)
			if err == nil {
				t.Fatal("an oversized request started a run")
			}
			if !strings.Contains(err.Error(), tc.want.String()) {
				t.Errorf("error = %q, want the daemon's %s", err, tc.want)
			}
		})
	}

	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %+v, want none for refused requests", runs)
	}
}

func TestRunRequestCannotReplaceTheDaemonsCheck(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	cfg := runConfig(s, newBlockingBackend())
	cfg.Check = "make test"
	r := start(t, Config{Runner: cfg})
	defer r.stop(t)
	client := &Client{Path: r.sock}

	_, err := client.Run(ctx, RunOptions{Check: "true"})
	if err == nil {
		t.Fatal("a different check was accepted")
	}
	if !strings.Contains(err.Error(), cfg.Check) {
		t.Errorf("error = %q, want the daemon's check %q", err, cfg.Check)
	}

	// The same check is not a replacement, so it is not refused.
	if _, err := client.Run(ctx, RunOptions{Check: cfg.Check}); err != nil {
		t.Fatalf("run with the daemon's check: %v", err)
	}
}

func TestRunRequestMaySupplyACheckWhenTheDaemonHasNone(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	r := start(t, Config{Runner: runConfig(s, newBlockingBackend())})
	defer r.stop(t)

	if _, err := (&Client{Path: r.sock}).Run(ctx, RunOptions{Check: "true"}); err != nil {
		t.Fatalf("run with a client check: %v", err)
	}
}

func TestRunResponseCarriesTheRunRecord(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	r := start(t, Config{Runner: runConfig(s, b)})
	defer r.stop(t)

	stats, err := (&Client{Path: r.sock}).Run(ctx, RunOptions{MaxTasks: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Run.ID == 0 || stats.Run.Owner == "" || stats.Run.EndedAt == "" {
		t.Fatalf("stats = %+v, want a finished run", stats.Run)
	}

	// The wire shape is the daemon's, not the runner's: a result that crosses
	// the socket is decodable without knowing the runner's structs.
	var wire struct {
		OK  bool `json:"ok"`
		Run struct {
			Run struct {
				ID      int64 `json:"id"`
				Workers int   `json:"workers"`
			} `json:"run"`
		} `json:"run"`
	}
	conn, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(`{"v":1,"do":"status"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := json.NewDecoder(conn).Decode(&wire); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !wire.OK {
		t.Error("status was refused")
	}
}
