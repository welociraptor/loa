package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"loa/internal/runner"
	"loa/internal/store"
)

func autoRunConfig(s *store.Store, b *blockingBackend) runner.Config {
	cfg := runConfig(s, b)
	cfg.Workers = 1
	cfg.MaxTasks = 1
	return cfg
}

// probeStub stands in for `herdr status`: the test decides when Herdr answers,
// so no real Herdr is needed to see what the scheduler does with each answer.
type probeStub struct {
	mu    sync.Mutex
	fail  bool
	calls int
}

func (p *probeStub) probe(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.fail {
		return errors.New("herdr status: no server")
	}
	return nil
}

func (p *probeStub) setFail(fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = fail
}

func (p *probeStub) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestAutoWaitsForASuccessfulProbe(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	addRunTask(t, s)

	probe := &probeStub{fail: true}
	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Interval: 20 * time.Millisecond,
		Probe: probe.probe,
	})
	defer r.stop(t)

	// The probe is retried every interval, not once at startup, so Herdr's
	// recovery is noticed without restarting the daemon.
	waitFor(t, "the probe to be retried each interval", func() bool { return probe.count() >= 3 })
	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want none while the probe fails", len(runs))
	}

	probe.setFail(false)
	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("auto did not start a run after the probe succeeded")
	}
}

func TestManualRunIsNotGatedByTheProbe(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	addRunTask(t, s)

	probe := &probeStub{fail: true}
	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Interval: 20 * time.Millisecond,
		Probe: probe.probe,
	})
	defer r.stop(t)

	client := &Client{Path: r.sock}
	result := make(chan error, 1)
	go func() {
		_, err := client.Run(ctx, RunOptions{MaxTasks: 1})
		result <- err
	}()

	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("a manual run was gated on the probe")
	}

	if err := client.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Error("a stopped manual run reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the manual run did not finish after stop")
	}
}

func TestAutoStartsARunWithinOneInterval(t *testing.T) {
	s := daemonStore(t)
	b := newBlockingBackend()
	addRunTask(t, s)

	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Interval: 20 * time.Millisecond,
	})
	defer r.stop(t)

	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("auto did not start a run within one interval")
	}
}

func TestPausePreventsAutoUntilResume(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	addRunTask(t, s)

	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Interval: 200 * time.Millisecond,
	})
	defer r.stop(t)

	client := &Client{Path: r.sock}
	if err := client.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Several intervals pass with a claimable task and no run.
	time.Sleep(500 * time.Millisecond)
	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("paused daemon started %d run(s)", len(runs))
	}

	if err := client.Resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("auto did not start after resume")
	}
}

func TestAutoDoesNotStartASecondRunWhileOneIsActive(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	addRunTask(t, s)
	addRunTask(t, s)

	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Interval: 20 * time.Millisecond,
	})
	defer r.stop(t)

	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("auto did not start the first run")
	}
	time.Sleep(100 * time.Millisecond)

	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want one active run", len(runs))
	}
	if !runs[0].Active() {
		t.Fatal("the first run finished; the test raced the backend")
	}
}

func TestDailySpentStopsStarting(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	addRunTask(t, s)

	seed, err := s.StartRun(ctx, "seed/1", 1, 1)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// A failed attempt spends the budget too: it leased a task and cost an agent.
	if err := s.FinishRun(ctx, seed.ID, 1, 1); err != nil {
		t.Fatalf("finish seed run: %v", err)
	}

	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Daily: 2,
		Interval: 20 * time.Millisecond,
	})
	defer r.stop(t)

	time.Sleep(100 * time.Millisecond)
	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want only the seeded run", len(runs))
	}

	tasks, err := s.List(ctx, store.New)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("queued tasks = %d, want the task still waiting", len(tasks))
	}
}

func TestAutoWaitsForIdleAfterTheLastSettledRun(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	addRunTask(t, s)

	seed, err := s.StartRun(ctx, "seed/1", 1, 1)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := s.FinishRun(ctx, seed.ID, 0, 0); err != nil {
		t.Fatalf("finish seed run: %v", err)
	}

	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Idle: time.Hour,
		Interval: 20 * time.Millisecond,
	})
	defer r.stop(t)

	time.Sleep(100 * time.Millisecond)
	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want only the seeded run", len(runs))
	}
}

func TestAutoIsQuietWhenNothingIsClaimable(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	addRunTask(t, s)

	task, err := s.Lease(ctx, "probe/1", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	// A failed attempt is queued for a later retry, so it is new but not
	// claimable now: the scheduler must not spin against it.
	if err := s.Fail(ctx, task.ID, "retry later"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Interval: 20 * time.Millisecond,
	})
	defer r.stop(t)

	time.Sleep(100 * time.Millisecond)
	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want none for a queue with nothing claimable", len(runs))
	}
}

func TestAutoReapsAnExpiredLease(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	task := addRunTask(t, s)

	// A lease from an hour ago, as a daemon that died mid-attempt leaves it.
	leased, err := s.Lease(ctx, "probe/1", time.Hour, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased == nil || leased.ID != task.ID {
		t.Fatalf("lease = %+v, want the task", leased)
	}

	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Interval: 20 * time.Millisecond,
	})
	defer r.stop(t)

	waitFor(t, "the scheduler to reap the lease in a run", func() bool {
		runs, err := s.Runs(ctx, 10)
		return err == nil && len(runs) == 1
	})
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != store.New || got.NotBefore.IsZero() {
		t.Errorf("task = %s with retry %v, want the dead lease reaped", got.Status, got.NotBefore)
	}
}

func TestPauseLeavesTheRunInFlightAlone(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	b := newBlockingBackend()
	task := addRunTask(t, s)

	r := start(t, Config{
		Runner: autoRunConfig(s, b), Auto: true, Interval: 20 * time.Millisecond,
	})
	defer r.stop(t)

	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("auto did not start a run")
	}

	client := &Client{Path: r.sock}
	if err := client.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Run == nil {
		t.Fatal("pause took the run in flight away")
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != store.Leased {
		t.Errorf("task status = %s, want the active attempt left alone", got.Status)
	}
}
