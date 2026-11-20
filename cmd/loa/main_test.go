package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"loa/internal/daemon"
	"loa/internal/runner"
	"loa/internal/store"
)

// The binary is built once, so these tests exercise argument parsing, exit
// codes and output the way a shell does, not the way a function call does.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loa-bin")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "loa")

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
	t   *testing.T
	db  string
	dir string
	env []string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	return &cli{t: t, db: filepath.Join(t.TempDir(), "tasks.db")}
}

func (c *cli) run(args ...string) (stdout, stderr string, code int) {
	c.t.Helper()
	cmd := exec.Command(binary, args...)
	// The tests describe a machine with no Herdr session. Keeping the caller's
	// environment would join rows to real workspaces, and a herdr on PATH would
	// answer ps with this machine's session.
	env := []string{"LOA_DB=" + c.db, "PATH=" + c.t.TempDir()}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HERDR_") || strings.HasPrefix(kv, "LOA_DB=") || strings.HasPrefix(kv, "PATH=") ||
			strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, c.env...)
	cmd.Env = env
	if c.dir != "" {
		cmd.Dir = c.dir
	}
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

// showField is the value after a label in `loa show`'s aligned output.
func showField(t *testing.T, show, label string) string {
	t.Helper()
	for _, line := range strings.Split(show, "\n") {
		if strings.HasPrefix(line, label) {
			return strings.TrimSpace(strings.TrimPrefix(line, label))
		}
	}
	t.Fatalf("show has no %q line:\n%s", label, show)
	return ""
}

// psRows keys the task rows of `loa ps` by id. An orphan has no task id, so it
// has no row here.
func psRows(t *testing.T, ps string) map[int64]string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(ps), "\n")
	rows := make(map[int64]string, len(lines))
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		rows[id] = line
	}
	return rows
}

// psIDs is the task ids `loa ps` printed, in order. Orphan rows have no id.
func psIDs(t *testing.T, ps string) []int64 {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(ps), "\n")
	ids := make([]int64, 0, len(lines)-1)
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if id, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// listColumn is a `loa list` row's value keyed by the header, so a test names a
// column instead of counting tabs.
func listColumn(t *testing.T, list, id, column string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(list, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("list has no rows:\n%s", list)
	}
	header := lines[0]
	start, end, offset := -1, -1, 0
	for _, name := range strings.Fields(header) {
		at := strings.Index(header[offset:], name) + offset
		if start >= 0 {
			end = at
			break
		}
		if name == column {
			start = at
		}
		offset = at + len(name)
	}
	if start < 0 {
		t.Fatalf("list has no %s column:\n%s", column, list)
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != id {
			continue
		}
		if end < 0 || end > len(line) {
			end = len(line)
		}
		return strings.TrimSpace(line[start:end])
	}
	t.Fatalf("list has no row for id %s:\n%s", id, list)
	return ""
}

func TestAddListLeaseDone(t *testing.T) {
	c := newCLI(t)

	if out := c.mustRun("add", "-workspace", "loa", "-title", "ship it", "-priority", "3", "-scope", "repo:demo"); strings.TrimSpace(out) != "1" {
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
	c.mustRun("done", "1", "-branch", "loa/1.1", "-result", "merged")

	done := c.mustRun("list", "-status", "done")
	if !strings.Contains(done, "ship it") || !strings.Contains(done, "1/2") {
		t.Errorf("done list = %q", done)
	}

	show := c.mustRun("show", "1")
	for _, want := range []string{"branch:", "loa/1.1", "result:", "merged", "already done"} {
		if !strings.Contains(show, want) {
			t.Errorf("show = %q, missing %q", show, want)
		}
	}
}

func TestListShowsNoDependencies(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "lonely")

	if got := listColumn(t, c.mustRun("list"), "1", "DEPS"); got != "-" {
		t.Errorf("DEPS = %q, want -", got)
	}
}

func TestListShowsDependenciesInIDOrder(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "root")
	c.mustRun("add", "-workspace", "loa", "-title", "middle", "-dep", "1")
	c.mustRun("add", "-workspace", "loa", "-title", "leaf", "-dep", "2")
	c.mustRun("add", "-workspace", "loa", "-title", "both", "-dep", "1", "-dep", "3")

	list := c.mustRun("list")
	for id, want := range map[string]string{"1": "-", "2": "1", "3": "2", "4": "1,3"} {
		if got := listColumn(t, list, id, "DEPS"); got != want {
			t.Errorf("task %s DEPS = %q, want %q", id, got, want)
		}
	}
}

func TestShowExplainsWhyATaskIsNotRunning(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "schema", "-scope", "repo:demo")
	c.mustRun("add", "-workspace", "loa", "-title", "writer", "-scope", "repo:demo", "-dep", "1")
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

func TestShowNamesADoneDependency(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "root")
	c.mustRun("add", "-workspace", "loa", "-title", "child", "-dep", "1")
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("done", "1", "-result", "finished")

	show := c.mustRun("show", "2")
	if got, want := showField(t, show, "depends on:"), `#1 (done) "root"`; got != want {
		t.Errorf("depends on = %q, want %q", got, want)
	}
	// The edge list is not the blocker explanation: a done dependency holds nothing.
	if strings.Contains(show, "waiting on #1") {
		t.Errorf("show names a done dependency as a blocker: %q", show)
	}
}

func TestShowPrintsAnEmptyDependentList(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "lonely")

	show := c.mustRun("show", "1")
	if got, want := showField(t, show, "needed by:"), "-"; got != want {
		t.Errorf("needed by = %q, want %q", got, want)
	}
}

func TestShowNamesBothDirections(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "root")
	c.mustRun("add", "-workspace", "loa", "-title", "middle", "-dep", "1")
	c.mustRun("add", "-workspace", "loa", "-title", "leaf", "-dep", "2")

	show := c.mustRun("show", "2")
	if got, want := showField(t, show, "depends on:"), `#1 (new) "root"`; got != want {
		t.Errorf("depends on = %q, want %q", got, want)
	}
	if got, want := showField(t, show, "needed by:"), `#3 (new) "leaf"`; got != want {
		t.Errorf("needed by = %q, want %q", got, want)
	}
}

func TestShowRejectsUnknownTask(t *testing.T) {
	c := newCLI(t)
	_, errOut, code := c.run("show", "42")
	if code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestCancelDeletesANewTaskAndPrintsIt(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "queued")

	out := c.mustRun("cancel", "1")
	for label, want := range map[string]string{
		"id:": "1", "title:": "queued", "status:": "new", "attempts:": "0/2",
	} {
		if got := showField(t, out, label); got != want {
			t.Errorf("cancel %s = %q, want %q", label, got, want)
		}
	}
	if list := c.mustRun("list"); strings.Contains(list, "queued") {
		t.Errorf("cancelled task is still listed: %q", list)
	}
}

func TestCancelRefusesALeasedTask(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "running")
	c.mustRun("lease", "-owner", "cli-test")

	_, errOut, code := c.run("cancel", "1")
	if code != 1 || !strings.Contains(errOut, "leased by cli-test until") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if list := c.mustRun("list", "-status", "leased"); !strings.Contains(list, "running") {
		t.Errorf("refused cancel removed the leased task: %q", list)
	}
}

func TestCancelRefusesADoneTask(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "shipped")
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("done", "1", "-branch", "loa/1.1")

	_, errOut, code := c.run("cancel", "1")
	if code != 1 || !strings.Contains(errOut, "task is done") || !strings.Contains(errOut, "loa/1.1") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestCancelRefusesATaskWithADependent(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "base")
	c.mustRun("add", "-workspace", "loa", "-title", "dependent", "-dep", "1")

	_, errOut, code := c.run("cancel", "1")
	if code != 1 || !strings.Contains(errOut, "#2") || !strings.Contains(errOut, "dependent") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if list := c.mustRun("list"); !strings.Contains(list, "base") || !strings.Contains(list, "dependent") {
		t.Errorf("refused cancel changed the queue: %q", list)
	}
}

func TestCancelDeletesABlockedTask(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "needs a human", "-max", "1")
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("fail", "1", "-result", "out of attempts")

	out := c.mustRun("cancel", "1")
	if got := showField(t, out, "status:"); got != "blocked" {
		t.Errorf("cancel status = %q, want blocked", got)
	}
	if list := c.mustRun("list"); strings.Contains(list, "needs a human") {
		t.Errorf("cancelled task is still listed: %q", list)
	}
}

func TestCancelRejectsUnknownTask(t *testing.T) {
	c := newCLI(t)
	_, errOut, code := c.run("cancel", "42")
	if code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestDoneRequiresALease(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "unclaimed")

	_, errOut, code := c.run("done", "1")
	if code != 1 || !strings.Contains(errOut, "not leased") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestFailRequeuesAndReportsRetry(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "flaky")
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

func TestUnblockRequeuesABlockedTask(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "needs a nudge", "-max", "1")
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("fail", "1", "-result", "waiting for approval")

	if list := c.mustRun("list", "-status", "blocked"); !strings.Contains(list, "needs a nudge") {
		t.Fatalf("the task did not block: %q", list)
	}
	c.mustRun("unblock", "1")

	var leased struct {
		ID     int64
		Status string
	}
	if err := json.Unmarshal([]byte(c.mustRun("lease", "-owner", "cli-test")), &leased); err != nil {
		t.Fatalf("lease output is not JSON: %v", err)
	}
	if leased.ID != 1 || leased.Status != "leased" {
		t.Errorf("lease after unblock = %+v", leased)
	}
}

func TestUnblockRejectsATaskThatIsNotBlocked(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "already queued")

	_, errOut, code := c.run("unblock", "1")
	if code != 1 || !strings.Contains(errOut, "not leased") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestResolveFailKeepsTheResultOnABlockedTask(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "approval needed", "-max", "1")
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("fail", "1", "-result", "waiting for approval")

	c.mustRun("resolve", "1", "-fail", "-result", "the change was rejected")

	show := c.mustRun("show", "1")
	if list := c.mustRun("list", "-status", "blocked"); !strings.Contains(list, "approval needed") {
		t.Errorf("the task is not blocked: %q", list)
	}
	if !strings.Contains(show, "the change was rejected") {
		t.Errorf("show = %q, missing the result", show)
	}
}

func TestResolveRejectsALeasedTask(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "still running")
	c.mustRun("lease", "-owner", "cli-test")

	_, errOut, code := c.run("resolve", "1")
	if code != 1 || !strings.Contains(errOut, "not leased") {
		t.Errorf("exit %d, stderr %q", code, errOut)
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

func TestPsPrintsTheQueueAndTheSessionColumns(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "queued", "-scope", "repo:demo")
	c.mustRun("add", "-workspace", "loa", "-title", "running", "-scope", "repo:other")
	c.mustRun("lease", "-owner", "cli-test")

	// With a session: the leased row names its workspace, if not yet its agent.
	out := c.mustRun("ps")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("ps = %q", out)
	}
	if !strings.Contains(lines[0], "SESSION") || !strings.Contains(lines[0], "WORKSPACE") {
		t.Errorf("header = %q", lines[0])
	}
	// Rows are located by id, not by position: ps is a separate order from the tab.
	rows := psRows(t, out)
	if !strings.Contains(rows[1], "leased") || !strings.Contains(rows[2], "new") {
		t.Errorf("rows = %q", lines[1:])
	}
}

func TestPsListsTasksNewestFirst(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "oldest")
	c.mustRun("add", "-workspace", "loa", "-title", "middle")
	c.mustRun("add", "-workspace", "loa", "-title", "newest")
	// Status must not decide the order: the oldest is leased, the middle is done.
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("done", "2", "-branch", "loa/2.1", "-result", "shipped")

	ids := psIDs(t, c.mustRun("ps"))
	if want := []int64{3, 2, 1}; !slices.Equal(ids, want) {
		t.Fatalf("ps ids = %v, want %v", ids, want)
	}

	var snap struct {
		Rows []struct {
			ID int64 `json:"id"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("ps", "-json")), &snap); err != nil {
		t.Fatalf("ps -json: %v", err)
	}
	jsonIDs := make([]int64, 0, len(snap.Rows))
	for _, row := range snap.Rows {
		jsonIDs = append(jsonIDs, row.ID)
	}
	if !slices.Equal(jsonIDs, ids) {
		t.Errorf("ps -json ids = %v, table ids = %v", jsonIDs, ids)
	}
}

func TestPsJSONCarriesTheJoin(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "queued")
	c.mustRun("lease", "-owner", "cli-test")

	var snap struct {
		Rows []struct {
			ID        int64  `json:"id"`
			Status    string `json:"status"`
			Workspace string `json:"workspace"`
			Session   string `json:"session"`
		} `json:"rows"`
		Orphans            []struct{ WorkspaceID string } `json:"orphans"`
		SessionUnavailable string                         `json:"session_unavailable"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("ps", "-json")), &snap); err != nil {
		t.Fatalf("ps -json: %v", err)
	}
	if len(snap.Rows) != 1 || snap.Rows[0].Status != "leased" || snap.Rows[0].Workspace != "loa" {
		t.Fatalf("rows = %+v", snap.Rows)
	}
	// "closed" would be a lie here: nothing was ever dispatched, so there is no
	// workspace to be closed.
	if snap.Rows[0].Session != "-" {
		t.Errorf("session = %q, want -", snap.Rows[0].Session)
	}
}

func TestReportPrintsTheDayAndTheGap(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "ship the report")
	c.mustRun("add", "-workspace", "loa", "-title", "waiting")
	c.mustRun("lease", "-owner", "cli-test")
	c.mustRun("done", "1", "-branch", "loa/1.1", "-result", "shipped it")

	out := c.mustRun("report")
	for _, want := range []string{"since ", "done: 1", "loa/1.1", "queued: 1", "activity:"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

func TestReportAcceptsADateWindow(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "yesterday")
	if out := c.mustRun("report", "-since", "2026-01-01"); !strings.Contains(out, "since ") {
		t.Errorf("report = %q", out)
	}
	if _, errOut, code := c.run("report", "-since", "tuesday"); code != 1 || !strings.Contains(errOut, "use a duration") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestReportJSONCarriesTheSections(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "queued")

	var r struct {
		Queued []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"queued"`
		Done []struct{ ID int64 } `json:"done"`
		Idle int64                `json:"idle_ns"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("report", "-json")), &r); err != nil {
		t.Fatalf("report -json: %v", err)
	}
	if len(r.Queued) != 1 || r.Queued[0].Title != "queued" || len(r.Done) != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestImportCreatesTasksWithDependencies(t *testing.T) {
	c := newCLI(t)
	file := filepath.Join(t.TempDir(), "proposal.json")
	doc := `{"tasks":[
		{"title":"schema","workspace":"loa","priority":3},
		{"title":"writer","workspace":"loa","after":["schema"]}
	]}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := c.mustRun("import", file)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("import printed %q, want two ids", out)
	}
	// The second task waits on the first, by title, before either had an id.
	show := c.mustRun("show", lines[1])
	if !strings.Contains(show, "waiting on #"+lines[0]) {
		t.Errorf("show = %q", show)
	}
}

func TestImportAppliesNothingWhenAnythingIsWrong(t *testing.T) {
	c := newCLI(t)
	file := filepath.Join(t.TempDir(), "proposal.json")
	doc := `{"tasks":[
		{"title":"fine","workspace":"loa"},
		{"title":"broken"}
	]}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, errOut, code := c.run("import", file)
	if code != 1 || !strings.Contains(errOut, "workspace is required") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if list := c.mustRun("list"); strings.Contains(list, "fine") {
		t.Errorf("a rejected document still created tasks:\n%s", list)
	}
}

func TestImportDryRunCreatesNothing(t *testing.T) {
	c := newCLI(t)
	file := filepath.Join(t.TempDir(), "proposal.json")
	if err := os.WriteFile(file, []byte(`[{"title":"later","workspace":"loa"}]`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := c.mustRun("import", "-dry-run", file)
	if !strings.Contains(out, "later") {
		t.Errorf("dry run printed %q", out)
	}
	if list := c.mustRun("list"); strings.Contains(list, "later") {
		t.Errorf("the dry run created a task:\n%s", list)
	}
}

// seedRun writes one run row directly, so the read-only commands can be tested
// without a Herdr session.
func seedRun(t *testing.T, db, owner string, done, failed int, active bool) store.Run {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	run, err := s.StartRun(ctx, owner, 2, 5)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if !active {
		if err := s.FinishRun(ctx, run.ID, done, failed); err != nil {
			t.Fatalf("finish run: %v", err)
		}
	}
	return run
}

func TestRunsListsNewestFirstAndMarksTheActiveRow(t *testing.T) {
	c := newCLI(t)
	seedRun(t, c.db, "owner/a", 1, 0, false)
	seedRun(t, c.db, "owner/b", 0, 0, true)

	out := c.mustRun("runs")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("runs = %q", out)
	}
	for _, want := range []string{"ID", "OWNER", "STARTED", "ENDED", "DONE", "FAILED", "BUDGET"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header %q is missing %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "owner/b") || !strings.Contains(lines[1], "- [active]") {
		t.Errorf("first row = %q, want the open run marked", lines[1])
	}
	if !strings.Contains(lines[2], "owner/a") || strings.Contains(lines[2], "[active]") {
		t.Errorf("second row = %q, want the finished run", lines[2])
	}
	if !strings.Contains(lines[1], "2/5") || !strings.Contains(lines[2], "2/5") {
		t.Errorf("rows = %q, want the workers/budget column", lines[1:])
	}

	limited := strings.Split(strings.TrimSpace(c.mustRun("runs", "-limit", "1")), "\n")
	if len(limited) != 2 || !strings.Contains(limited[1], "owner/b") {
		t.Errorf("runs -limit 1 = %q", limited)
	}
}

func TestRunsJSONCarriesEveryRun(t *testing.T) {
	c := newCLI(t)
	first := seedRun(t, c.db, "owner/a", 1, 0, false)
	second := seedRun(t, c.db, "owner/b", 0, 0, true)

	var got struct {
		Runs []struct {
			ID        int64  `json:"id"`
			Owner     string `json:"owner"`
			StartedAt string `json:"started_at"`
			EndedAt   string `json:"ended_at"`
			Workers   int    `json:"workers"`
			MaxTasks  int    `json:"max_tasks"`
			Done      int    `json:"done"`
			Failed    int    `json:"failed"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("runs", "-json")), &got); err != nil {
		t.Fatalf("runs -json: %v", err)
	}
	if len(got.Runs) != 2 {
		t.Fatalf("runs = %+v", got.Runs)
	}
	newest, older := got.Runs[0], got.Runs[1]
	if newest.ID != second.ID || newest.Owner != "owner/b" || newest.EndedAt != "" {
		t.Errorf("newest = %+v, want the active run", newest)
	}
	if older.ID != first.ID || older.Owner != "owner/a" || older.Done != 1 || older.EndedAt == "" {
		t.Errorf("older = %+v, want the finished run", older)
	}
	if newest.Workers != 2 || newest.MaxTasks != 5 || older.Workers != 2 || older.MaxTasks != 5 {
		t.Errorf("budget fields = %+v / %+v", newest, older)
	}
}

func TestRunsCloseSettlesADeadRun(t *testing.T) {
	c := newCLI(t)
	dead := seedRun(t, c.db, "owner/a", 0, 0, true)

	out := c.mustRun("runs", "-close", strconv.FormatInt(dead.ID, 10))
	if !strings.Contains(out, "owner/a") || strings.Contains(out, "[active]") {
		t.Errorf("runs -close printed %q, want the settled run", out)
	}

	var got struct {
		Runs []struct {
			ID      int64  `json:"id"`
			EndedAt string `json:"ended_at"`
			Done    int    `json:"done"`
			Failed  int    `json:"failed"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("runs", "-json")), &got); err != nil {
		t.Fatalf("runs -json: %v", err)
	}
	if len(got.Runs) != 1 || got.Runs[0].ID != dead.ID || got.Runs[0].EndedAt == "" {
		t.Fatalf("runs = %+v, want the run closed", got.Runs)
	}
	if got.Runs[0].Done != 0 || got.Runs[0].Failed != 0 {
		t.Errorf("counts = (%d, %d), want the recorded (0, 0)", got.Runs[0].Done, got.Runs[0].Failed)
	}

	// The slot is free: the next run starts instead of being refused.
	next := seedRun(t, c.db, "owner/b", 0, 0, true)
	if next.ID == dead.ID {
		t.Errorf("new run reuses id %d", dead.ID)
	}

	// A finished run is not settled twice, and the refusal names the state.
	_, errOut, code := c.run("runs", "-close", strconv.FormatInt(dead.ID, 10))
	if code != 1 || !strings.Contains(errOut, "finished") {
		t.Errorf("exit %d, stderr %q, want a finished-run refusal", code, errOut)
	}
}

func TestRunJSONShape(t *testing.T) {
	stats := runner.Stats{
		Run: store.Run{
			ID: 12, Owner: "host/pid", StartedAt: "2026-11-19T10:00:00Z",
			EndedAt: "2026-11-19T10:05:00Z", Workers: 4, MaxTasks: 4, Done: 1, Failed: 2,
		},
		Outcomes: []runner.TaskOutcome{
			{ID: 1, Done: true, Branch: "loa/1.1", WorkspaceID: "w4", PaneID: "w4:p1", Verdict: "approve", CheckPassed: true},
			{ID: 2, Blocked: true, Branch: "loa/2.1", WorkspaceID: "w5", PaneID: "w5:p1"},
			{ID: 3},
		},
	}

	var buf bytes.Buffer
	if err := printRunJSON(&buf, stats); err != nil {
		t.Fatalf("print: %v", err)
	}

	var got struct {
		Run struct {
			ID        int64  `json:"id"`
			Owner     string `json:"owner"`
			StartedAt string `json:"started_at"`
			EndedAt   string `json:"ended_at"`
			Workers   int    `json:"workers"`
			MaxTasks  int    `json:"max_tasks"`
			Done      int    `json:"done"`
			Failed    int    `json:"failed"`
		} `json:"run"`
		Tasks []struct {
			ID          int64  `json:"id"`
			Status      string `json:"status"`
			Branch      string `json:"branch"`
			WorkspaceID string `json:"workspace_id"`
			PaneID      string `json:"pane_id"`
			Blocked     bool   `json:"blocked"`
			Verdict     string `json:"verdict"`
			CheckPassed bool   `json:"check_passed"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("run json: %v: %s", err, buf.String())
	}

	if got.Run.ID != 12 || got.Run.Owner != "host/pid" || got.Run.Workers != 4 ||
		got.Run.MaxTasks != 4 || got.Run.Done != 1 || got.Run.Failed != 2 ||
		got.Run.StartedAt == "" || got.Run.EndedAt == "" {
		t.Errorf("run = %+v", got.Run)
	}
	if len(got.Tasks) != 3 {
		t.Fatalf("tasks = %+v", got.Tasks)
	}
	want := []struct {
		id      int64
		status  string
		branch  string
		ws      string
		pane    string
		block   bool
		verdict string
		check   bool
	}{
		{1, "done", "loa/1.1", "w4", "w4:p1", false, "approve", true},
		{2, "blocked", "loa/2.1", "w5", "w5:p1", true, "", false},
		{3, "failed", "", "", "", false, "", false},
	}
	for i, w := range want {
		task := got.Tasks[i]
		if task.ID != w.id || task.Status != w.status || task.Branch != w.branch ||
			task.WorkspaceID != w.ws || task.PaneID != w.pane || task.Blocked != w.block ||
			task.Verdict != w.verdict || task.CheckPassed != w.check {
			t.Errorf("task %d = %+v, want %+v", i, task, w)
		}
	}
}

func TestRunJSONWithoutADaemonExitsOneAndPrintsNothing(t *testing.T) {
	c := newCLI(t)
	stdout, stderr, code := c.run("run", "-json")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want no document when the run never started", stdout)
	}
	if !strings.Contains(stderr, "daemon") && !strings.Contains(stderr, "XDG_RUNTIME_DIR") {
		t.Errorf("stderr = %q, want the daemon connection reason", stderr)
	}
}

func TestRunSendsRelativePromptsAsAnAbsolutePath(t *testing.T) {
	runtimeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runtimeDir, "loa"), 0o700); err != nil {
		t.Fatalf("runtime dir: %v", err)
	}
	listener, err := net.Listen("unix", filepath.Join(runtimeDir, "loa", "sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	type served struct {
		req daemon.Request
		err error
	}
	servedCh := make(chan served, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			servedCh <- served{err: err}
			return
		}
		defer func() { _ = conn.Close() }()
		var req daemon.Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			servedCh <- served{err: err}
			return
		}
		resp := daemon.Response{V: daemon.Protocol, OK: true, Run: &daemon.RunResult{Run: daemon.RunRecord{ID: 1}}}
		if err := json.NewEncoder(conn).Encode(resp); err != nil {
			servedCh <- served{err: err}
			return
		}
		servedCh <- served{req: req}
	}()

	c := newCLI(t)
	c.dir = t.TempDir()
	c.env = []string{"XDG_RUNTIME_DIR=" + runtimeDir}
	if _, stderr, code := c.run("run", "-prompts", "prompts"); code != 0 {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	select {
	case got := <-servedCh:
		if got.err != nil {
			t.Fatalf("fake daemon: %v", got.err)
		}
		if got.req.Run == nil {
			t.Fatalf("request carried no run options: %+v", got.req)
		}
		want := filepath.Join(c.dir, "prompts")
		if got.req.Run.PromptsDir != want {
			t.Errorf("prompts = %q, want %q", got.req.Run.PromptsDir, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake daemon saw no request")
	}
}

func TestRoleIsAddedAndShown(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "plan the phase", "-role", "planner")

	list := c.mustRun("list")
	if !strings.Contains(list, "ROLE") || !strings.Contains(list, "planner") {
		t.Errorf("list = %q, want the role", list)
	}

	show := c.mustRun("show", "1")
	if !strings.Contains(show, "role:") || !strings.Contains(show, "planner") {
		t.Errorf("show = %q, want the role", show)
	}

	ps := c.mustRun("ps")
	if !strings.Contains(ps, "ROLE") || !strings.Contains(ps, "planner") {
		t.Errorf("ps = %q, want the role", ps)
	}

	var snap struct {
		Rows []struct {
			Role string `json:"role"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("ps", "-json")), &snap); err != nil {
		t.Fatalf("ps -json: %v", err)
	}
	if len(snap.Rows) != 1 || snap.Rows[0].Role != "planner" {
		t.Errorf("rows = %+v, want the role", snap.Rows)
	}
}

func TestDefaultRoleIsImplement(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "old style")
	if show := c.mustRun("show", "1"); !strings.Contains(show, "role:") || !strings.Contains(show, "implement") {
		t.Errorf("show = %q, want the default role", show)
	}
}

func TestShowRendersTheVerdictAndItsNotes(t *testing.T) {
	c := newCLI(t)
	c.mustRun("add", "-workspace", "loa", "-title", "review it", "-role", "review")

	s, err := store.Open(context.Background(), c.db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	if _, err := s.Lease(context.Background(), "test", time.Hour, time.Now()); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := s.CompleteReview(context.Background(), 1, "changes",
		`[{"file":"main.go","issue":"leaks a handle","suggestion":"close it"}]`,
		"verdict: changes"); err != nil {
		t.Fatalf("complete review: %v", err)
	}

	show := c.mustRun("show", "1")
	for _, want := range []string{"verdict:", "changes", "note:", "main.go: leaks a handle", "suggested:", "close it"} {
		if !strings.Contains(show, want) {
			t.Errorf("show = %q, missing %q", show, want)
		}
	}
}

func TestImportPhasesCompilesTheBarrierAndNamesThePlan(t *testing.T) {
	c := newCLI(t)
	file := filepath.Join(t.TempDir(), "plan-example.json")
	doc := `{"phases":[
		{"name":"schema","tasks":[{"title":"schema","workspace":"loa"}]},
		{"name":"writer","tasks":[{"title":"writer","workspace":"loa"}]}
	]}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := strings.Split(strings.TrimSpace(c.mustRun("import", file)), "\n")
	if len(out) != 2 {
		t.Fatalf("import printed %q, want two ids", out)
	}

	show := c.mustRun("show", out[1])
	for _, want := range []string{"waiting on #" + out[0], "plan:", "plan-example.json"} {
		if !strings.Contains(show, want) {
			t.Errorf("show = %q, missing %q", show, want)
		}
	}

	report := c.mustRun("report")
	if !strings.Contains(report, "plan: plan-example.json") {
		t.Errorf("report does not name the plan:\n%s", report)
	}

	var r struct {
		Queued []struct {
			Plan string `json:"plan"`
		} `json:"queued"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("report", "-json")), &r); err != nil {
		t.Fatalf("report -json: %v", err)
	}
	if len(r.Queued) != 2 || r.Queued[0].Plan != "plan-example.json" {
		t.Errorf("report plans = %+v", r.Queued)
	}
}

func TestImportRefusesAPhaseCycleAndCreatesNothing(t *testing.T) {
	c := newCLI(t)
	file := filepath.Join(t.TempDir(), "cycle.json")
	doc := `{"phases":[
		{"name":"one","after":"two","tasks":[{"title":"a","workspace":"loa"}]},
		{"name":"two","tasks":[{"title":"b","workspace":"loa"}]}
	]}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, errOut, code := c.run("import", file)
	if code != 1 || !strings.Contains(errOut, "dependency cycle") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if list := c.mustRun("list"); strings.Contains(list, "a") || strings.Contains(list, "b") {
		t.Errorf("a refused plan created tasks:\n%s", list)
	}
}

func TestPlanRejectsABadWorkflowBeforeLookingForASession(t *testing.T) {
	t.Setenv("LOA_DB", filepath.Join(t.TempDir(), "tasks.db"))
	t.Setenv("HERDR_ENV", "")
	workflow := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(workflow, []byte(`{
		"prompts": {"plan": "prompts/planner.md"},
		"check": "go test ./...",
		"merge": "auto",
		"stages": ["plan"],
		"cheks": "typo"
	}`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	err := cmdPlan(context.Background(), []string{"-workflow", workflow, "ship the report"})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want the workflow typo named", err)
	}
}

func TestPlanWritesUnderProposalsAndDoesNotApply(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	outDir := filepath.Join(dir, "proposals")
	db := filepath.Join(dir, "tasks.db")
	cwdFile := filepath.Join(dir, "scratch-cwd")

	// The fake Herdr answers the five calls `plan` makes. On prompt it writes the
	// plan into the scratch directory Herdr was asked to open, so the test sees
	// exactly what a real planner would leave.
	script := `#!/bin/sh
cmd="$1 $2"
case "$cmd" in
  "status --json") printf '%s\n' '{"client":{"version":"0.9.5"},"server":{"running":true,"compatible":true,"protocol":22}}' ;;
  "workspace create")
    shift 2
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --cwd) printf '%s' "$2" > "$PLAN_CWD_FILE"; shift 2 ;;
        --label) shift 2 ;;
        *) shift ;;
      esac
    done
    printf '%s\n' '{"id":"cli:workspace:create","result":{"workspace":{"workspace_id":"w1"},"root_pane":{"pane_id":"w1:p1"}}}'
    ;;
  "agent start") printf '%s\n' '{"id":"cli:agent:start","result":{"type":"ok"}}' ;;
  "agent prompt")
    printf '%s' '{"phases":[{"name":"one","tasks":[{"title":"schemas","workspace":"loa"}]}]}' > "$(cat "$PLAN_CWD_FILE")/plan.json"
    printf '%s\n' '{"id":"cli:agent:prompt","result":{"type":"ok"}}'
    ;;
  "workspace close") printf '%s\n' '{"id":"cli:workspace:close","result":{"type":"ok"}}' ;;
  *) printf 'unexpected: %s\n' "$*" >&2; exit 9 ;;
esac
`
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "herdr"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatalf("write fake herdr: %v", err)
	}
	workflow := filepath.Join(dir, "workflow.json")
	if err := os.WriteFile(workflow, []byte(`{
		"prompts": {"plan": "prompts/planner.md"},
		"check": "go test ./...",
		"merge": "auto",
		"stages": ["plan"]
	}`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	t.Setenv("HERDR_ENV", "1")
	t.Setenv("LOA_DB", db)
	t.Setenv("PLAN_CWD_FILE", cwdFile)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	if err := cmdPlan(context.Background(), []string{
		"-workflow", workflow, "-out", outDir, "ship the report",
	}); err != nil {
		t.Fatalf("plan: %v", err)
	}

	plans, err := filepath.Glob(filepath.Join(outDir, "plan-*.json"))
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %v, %v; want one written under proposals", plans, err)
	}
	raw, err := os.ReadFile(plans[0]) //nolint:gosec // G304: the path is the file the test just made
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if !strings.Contains(string(raw), `"phases"`) {
		t.Errorf("saved plan = %s", raw)
	}

	ctx := context.Background()
	s, err := store.Open(ctx, db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	tasks, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("plan applied %d task(s): %+v", len(tasks), tasks)
	}
}

func TestDBMismatchNamesBothPaths(t *testing.T) {
	if got := dbMismatch("/a/tasks.db", "/a/tasks.db"); got != "" {
		t.Errorf("equal paths = %q, want no mismatch", got)
	}
	if got := dbMismatch("/a/tasks.db", ""); got != "" {
		t.Errorf("an unreported daemon database = %q, want no invented mismatch", got)
	}
	got := dbMismatch("/tab/tasks.db", "/daemon/tasks.db")
	for _, want := range []string{"/tab/tasks.db", "/daemon/tasks.db", "run and stop are refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("mismatch = %q, missing %q", got, want)
		}
	}
}
