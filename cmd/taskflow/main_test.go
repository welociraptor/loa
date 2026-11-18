package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The binary is built once, so these tests exercise argument parsing, exit
// codes and output the way a shell does, not the way a function call does.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "taskflow-bin")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "taskflow")

	build := exec.Command("go", "build", "-o", binary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type cli struct {
	t  *testing.T
	db string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	return &cli{t: t, db: filepath.Join(t.TempDir(), "tasks.db")}
}

func (c *cli) run(args ...string) (stdout, stderr string, code int) {
	c.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "TASKFLOW_DB="+c.db)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			c.t.Fatalf("run %v: %v", args, err)
		}
		return out.String(), errb.String(), exit.ExitCode()
	}
	return out.String(), errb.String(), 0
}

func (c *cli) mustRun(args ...string) string {
	c.t.Helper()
	out, errOut, code := c.run(args...)
	if code != 0 {
		c.t.Fatalf("%v exited %d: %s", args, code, errOut)
	}
	return out
}

func TestAddListLeaseDone(t *testing.T) {
	c := newCLI(t)

	if out := c.mustRun("add", "-title", "ship it", "-priority", "3", "-scope", "repo:demo"); strings.TrimSpace(out) != "1" {
		t.Fatalf("add printed %q, want the new id", out)
	}

	list := c.mustRun("list")
	if !strings.Contains(list, "ship it") || !strings.Contains(list, "new") {
		t.Errorf("list = %q", list)
	}

	var leased struct {
		ID     int64
		Status string
		Scopes []string
	}
	if err := json.Unmarshal([]byte(c.mustRun("lease", "-owner", "cli-test")), &leased); err != nil {
		t.Fatalf("lease output is not JSON: %v", err)
	}
	if leased.ID != 1 || leased.Status != "leased" || len(leased.Scopes) != 1 {
		t.Errorf("lease = %+v", leased)
	}

	if empty := c.mustRun("lease", "-owner", "cli-test"); strings.TrimSpace(empty) != "null" {
		t.Errorf("second lease = %q, want null", empty)
	}

	// Flags after the positional argument, which the stdlib flag package does
	// not support on its own.
	c.mustRun("done", "1", "-branch", "taskflow/1.1", "-result", "merged")

	done := c.mustRun("list", "-status", "done")
	if !strings.Contains(done, "ship it") || !strings.Contains(done, "1/2") {
		t.Errorf("done list = %q", done)
	}

	show := c.mustRun("show", "1")
	for _, want := range []string{"branch:", "taskflow/1.1", "result:", "merged", "already done"} {
		if !strings.Contains(show, want) {
			t.Errorf("show = %q, missing %q", show, want)
		}
	}
}

func TestShowExplainsWhyATaskIsNotRunning(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-title", "schema", "-scope", "repo:demo")
	c.mustRun("add", "-title", "writer", "-scope", "repo:demo", "-dep", "1")
	c.mustRun("lease", "-owner", "cli-test")

	show := c.mustRun("show", "2")
	if !strings.Contains(show, "waiting on #1") {
		t.Errorf("show = %q", show)
	}
	if !strings.Contains(show, "scope \"repo:demo\" is held by #1") {
		t.Errorf("show = %q", show)
	}
	if strings.Contains(show, "runnable:\tyes") {
		t.Errorf("show claims a blocked task is runnable: %q", show)
	}
}

func TestShowRejectsUnknownTask(t *testing.T) {
	c := newCLI(t)
	_, errOut, code := c.run("show", "42")
	if code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestDoneRequiresALease(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-title", "unclaimed")

	_, errOut, code := c.run("done", "1")
	if code != 1 || !strings.Contains(errOut, "not leased") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestFailRequeuesAndReportsRetry(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-title", "flaky")
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("fail", "1", "-result", "tests were red")

	list := c.mustRun("list", "-status", "new")
	if !strings.Contains(list, "flaky") {
		t.Errorf("list = %q", list)
	}
	show := c.mustRun("show", "1")
	if !strings.Contains(show, "retry delayed until") || !strings.Contains(show, "tests were red") {
		t.Errorf("show = %q", show)
	}
}

func TestUsageErrorsExitWithTwo(t *testing.T) {
	c := newCLI(t)
	if _, errOut, code := c.run("nonsense"); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if _, _, code := c.run(); code != 2 {
		t.Errorf("no arguments exited %d, want 2", code)
	}
	if _, _, code := c.run("show"); code != 1 {
		t.Errorf("show without an id exited %d, want 1", code)
	}
}
