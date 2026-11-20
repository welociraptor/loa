package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"loa/internal/store"
)

func hostName(t *testing.T) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	return host
}

func seedActiveRun(t *testing.T, s *store.Store, owner string) store.Run {
	t.Helper()
	run, err := s.StartRun(context.Background(), owner, 2, 2)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	return run
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a process to leave dead: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for the dead process: %v", err)
	}
	return pid
}

func TestReconcileLeavesAnActiveRunWithALiveOwner(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	run := seedActiveRun(t, s, fmt.Sprintf("%s/%d", hostName(t), os.Getpid()))

	var logs lockedBuffer
	r := start(t, Config{
		Logger: log.New(&logs, "", 0),
		Runner: runConfig(s, newBlockingBackend()),
	})
	defer r.stop(t)

	active, err := s.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active == nil || active.ID != run.ID {
		t.Fatalf("active run = %+v, want run %d left alone", active, run.ID)
	}
	got := logs.String()
	if !strings.Contains(got, "leaving run") || !strings.Contains(got, "held by a live owner") {
		t.Errorf("logs = %q, want the live owner named", got)
	}
	if !strings.Contains(got, "loa runs -close") {
		t.Errorf("logs = %q, want the way to close it named", got)
	}
}

func TestReconcileSettlesAnActiveRunWithADeadOwner(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	pid := deadPID(t)
	run := seedActiveRun(t, s, fmt.Sprintf("%s/%d", hostName(t), pid))

	var logs lockedBuffer
	r := start(t, Config{
		Logger: log.New(&logs, "", 0),
		Runner: runConfig(s, newBlockingBackend()),
	})
	defer r.stop(t)

	active, err := s.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active != nil {
		t.Fatalf("active run = %+v, want the dead run settled", active)
	}
	settled, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if settled.Active() || settled.EndedAt == "" {
		t.Errorf("run = %+v, want it closed", settled)
	}
	got := logs.String()
	if !strings.Contains(got, "settled dead run") || !strings.Contains(got, strconv.Itoa(pid)) {
		t.Errorf("logs = %q, want the settled dead pid named", got)
	}
}

func TestReconcileLeavesAnUnparseableOwnerAlone(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	run := seedActiveRun(t, s, "owner-without-a-pid")

	var logs lockedBuffer
	r := start(t, Config{
		Logger: log.New(&logs, "", 0),
		Runner: runConfig(s, newBlockingBackend()),
	})
	defer r.stop(t)

	active, err := s.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active == nil || active.ID != run.ID {
		t.Fatalf("active run = %+v, want run %d left alone", active, run.ID)
	}
	got := logs.String()
	if !strings.Contains(got, "cannot parse owner") || !strings.Contains(got, "leaving run") {
		t.Errorf("logs = %q, want the unparseable owner named", got)
	}
}

func TestReconcileSettlesAnOwnerOnAnotherHost(t *testing.T) {
	ctx := context.Background()
	s := daemonStore(t)
	run := seedActiveRun(t, s, fmt.Sprintf("another-host/%d", os.Getpid()))

	var logs lockedBuffer
	r := start(t, Config{
		Logger: log.New(&logs, "", 0),
		Runner: runConfig(s, newBlockingBackend()),
	})
	defer r.stop(t)

	active, err := s.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active != nil {
		t.Fatalf("active run = %+v, want the other host's run settled", active)
	}
	settled, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if settled.Active() {
		t.Errorf("run = %+v, want it closed", settled)
	}
	if !strings.Contains(logs.String(), "not this host") {
		t.Errorf("logs = %q, want the host mismatch named", logs.String())
	}
}
