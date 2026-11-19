// Command loa is the human- and script-facing edge of the task store.
// The store is the contract; this CLI is a thin shell over it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"loa/internal/dash"
	"loa/internal/herdr"
	"loa/internal/live"
	"loa/internal/report"
	"loa/internal/runner"
	"loa/internal/store"
	"loa/internal/taskspec"
	"loa/internal/triage"
)

const usage = `loa — task queue for a multi-agent workflow

usage:
  loa add    -title TEXT -workspace LABEL [-body TEXT] [-priority N] [-max N] [-scope SCOPE]... [-dep ID]...
  loa list   [-status STATUS]...
  loa show   ID
  loa ps     [-json]               # the queue joined with the live session
  loa report [-since 24h|-since 2026-11-18] [-json]   # what the day did
  loa import [FILE|-] [-dry-run]   # apply a proposal: tasks, and priority changes
  loa triage [-timeout 10m]        # ask an agent to propose one
  loa lease  [-owner NAME] [-ttl 1h]
  loa unblock ID
  loa resolve ID [-fail] [-result TEXT]
  loa done   ID [-branch BRANCH] [-result TEXT]
  loa fail   ID [-result TEXT]
  loa run    [-workers N] [-max N] [-timeout 45m] [-json]
  loa runs   [-limit N] [-json] [-close ID]   # every run, newest first; -close settles a dead one
  loa dash   [-workers N] [-max N] # the tab: state, and the keys to act

all commands accept -db PATH (default $LOA_DB, else loa.db)`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "add":
		err = cmdAdd(ctx, os.Args[2:])
	case "list":
		err = cmdList(ctx, os.Args[2:])
	case "show":
		err = cmdShow(ctx, os.Args[2:])
	case "ps":
		err = cmdPs(ctx, os.Args[2:])
	case "report":
		err = cmdReport(ctx, os.Args[2:])
	case "import":
		err = cmdImport(ctx, os.Args[2:])
	case "triage":
		err = cmdTriage(ctx, os.Args[2:])
	case "lease":
		err = cmdLease(ctx, os.Args[2:])
	case "unblock":
		err = cmdUnblock(ctx, os.Args[2:])
	case "resolve":
		err = cmdResolve(ctx, os.Args[2:])
	case "done":
		err = cmdFinish(ctx, os.Args[2:], true)
	case "fail":
		err = cmdFinish(ctx, os.Args[2:], false)
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "runs":
		err = cmdRuns(ctx, os.Args[2:])
	case "dash":
		err = cmdDash(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "loa: unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "loa: %v\n", err)
		os.Exit(1)
	}
}

func defaultDB() string {
	if p := os.Getenv("LOA_DB"); p != "" {
		return p
	}
	return "loa.db"
}

// open builds a flag set for one command, parses args, and returns the opened
// store along with the positional arguments.
func open(ctx context.Context, name string, args []string, register func(*flag.FlagSet)) (*store.Store, []string, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	db := fs.String("db", defaultDB(), "path to the task database")
	register(fs)

	positional, err := parseFlags(fs, args)
	if err != nil {
		return nil, nil, err
	}
	s, err := store.Open(ctx, *db)
	if err != nil {
		return nil, nil, err
	}
	return s, positional, nil
}

// parseFlags allows flags to appear after positional arguments, which the
// stdlib flag package does not do on its own.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func cmdAdd(ctx context.Context, args []string) error {
	var scopes listFlag
	var deps listFlag
	var (
		title     string
		body      string
		workspace string
		priority  int
		maxTries  int
	)
	s, _, err := open(ctx, "add", args, func(fs *flag.FlagSet) {
		fs.StringVar(&title, "title", "", "task title")
		fs.StringVar(&body, "body", "", "task body handed to the agent")
		fs.StringVar(&workspace, "workspace", "", "Herdr workspace label the task belongs to")
		fs.IntVar(&priority, "priority", 0, "higher runs first")
		fs.IntVar(&maxTries, "max", 2, "attempts before the task is blocked")
		fs.Var(&scopes, "scope", "mutual-exclusion domain; repeatable")
		fs.Var(&deps, "dep", "id of a task that must be done first; repeatable")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	ids := make([]int64, 0, len(deps))
	for _, d := range deps {
		id, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			return fmt.Errorf("-dep %q: %w", d, err)
		}
		ids = append(ids, id)
	}

	id, err := s.Add(ctx, store.NewTask{
		Title: title, Body: body, Workspace: workspace, Priority: priority, MaxTries: maxTries,
		Scopes: scopes, Deps: ids,
	})
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func cmdList(ctx context.Context, args []string) error {
	var statuses listFlag
	s, positional, err := open(ctx, "list", args, func(fs *flag.FlagSet) {
		fs.Var(&statuses, "status", "filter by status (new, leased, done, blocked); repeatable")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}

	sf := make([]store.Status, len(statuses))
	for i, st := range statuses {
		sf[i] = store.Status(st)
	}
	tasks, err := s.List(ctx, sf...)
	if err != nil {
		return err
	}

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tSTATUS\tWORKSPACE\tPRI\tTRIES\tSCOPES\tTITLE"); err != nil {
		return err
	}
	for _, t := range tasks {
		scopes := "-"
		if len(t.Scopes) > 0 {
			scopes = strings.Join(t.Scopes, ",")
		}
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d/%d\t%s\t%s\n",
			t.ID, t.Status, workspaceOf(&t), t.Priority, t.Attempts, t.MaxAttempts, scopes, t.Title); err != nil {
			return err
		}
	}
	// A tabwriter buffers, so a write failure can surface only here or above.
	return w.Flush()
}

func cmdShow(ctx context.Context, args []string) error {
	s, positional, err := open(ctx, "show", args, func(*flag.FlagSet) {})
	if err != nil {
		return err
	}
	defer s.Close()

	id, err := taskID("show", positional)
	if err != nil {
		return err
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	blockers, err := s.Blockers(ctx, id, time.Now())
	if err != nil {
		return err
	}

	lines := []string{
		fmt.Sprintf("id:\t%d", task.ID),
		fmt.Sprintf("title:\t%s", task.Title),
		fmt.Sprintf("status:\t%s", task.Status),
		fmt.Sprintf("priority:\t%d", task.Priority),
		fmt.Sprintf("attempts:\t%d/%d", task.Attempts, task.MaxAttempts),
	}
	if len(task.Scopes) > 0 {
		lines = append(lines, fmt.Sprintf("scopes:\t%s", strings.Join(task.Scopes, ", ")))
	}
	if task.Status == store.Leased {
		lines = append(lines, fmt.Sprintf("lease:\t%s until %s", task.LeaseOwner, task.LeaseExpiry.Format(time.RFC3339)))
	}
	if !task.NotBefore.IsZero() {
		lines = append(lines, fmt.Sprintf("not before:\t%s", task.NotBefore.Format(time.RFC3339)))
	}
	if task.Branch != "" {
		lines = append(lines, fmt.Sprintf("branch:\t%s", task.Branch))
	}
	if task.RunMS > 0 {
		lines = append(lines, fmt.Sprintf("ran:\t%s", (time.Duration(task.RunMS)*time.Millisecond).Round(time.Second)))
	}
	if task.Result != "" {
		lines = append(lines, fmt.Sprintf("result:\t%s", task.Result))
	}
	if body := strings.TrimSpace(task.Body); body != "" {
		lines = append(lines, fmt.Sprintf("body:\t%s", body))
	}
	if len(blockers) == 0 {
		lines = append(lines, "runnable:\tyes")
	}
	for _, b := range blockers {
		lines = append(lines, fmt.Sprintf("blocked by:\t%s", b))
	}

	// One write, one error: a tabwriter only reports a failed write on Flush.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := io.WriteString(w, strings.Join(lines, "\n")+"\n"); err != nil {
		return err
	}
	return w.Flush()
}

type psJSON struct {
	Rows               []psRow    `json:"rows"`
	Orphans            []psOrphan `json:"orphans"`
	SessionUnavailable string     `json:"session_unavailable,omitempty"`
}

type psRow struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Workspace   string `json:"workspace"`
	Priority    int    `json:"priority"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	Branch      string `json:"branch,omitempty"`
	Session     string `json:"session"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	AgentName   string `json:"agent_name,omitempty"`
	AgentState  string `json:"agent_state,omitempty"`
	Result      string `json:"result,omitempty"`
}

type psOrphan struct {
	TaskID      int64  `json:"task_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

// cmdPs is the read the dashboard will sit on: the queue, joined with what the
// session says is happening to each task right now.
func cmdPs(ctx context.Context, args []string) error {
	var asJSON bool
	s, positional, err := open(ctx, "ps", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "emit the snapshot as JSON")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}

	// Without a session the store half is still worth printing; live.Read says so.
	session := &herdr.Client{}
	snap, err := live.Read(ctx, s, session)
	if err != nil {
		return err
	}

	if asJSON {
		out := psJSON{SessionUnavailable: snap.Unavailable}
		for _, row := range snap.Rows {
			out.Rows = append(out.Rows, psRow{
				ID: row.Task.ID, Title: row.Task.Title, Status: string(row.Task.Status),
				Workspace: row.Task.Workspace, Priority: row.Task.Priority,
				Attempts: row.Task.Attempts, MaxAttempts: row.Task.MaxAttempts,
				Branch: row.Task.Branch, Session: row.Session(),
				WorkspaceID: row.WorkspaceID, AgentName: row.AgentName, AgentState: row.AgentState,
				Result: row.Task.Result,
			})
		}
		for _, o := range snap.Orphans {
			out.Orphans = append(out.Orphans, psOrphan(o))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if snap.Unavailable != "" {
		fmt.Fprintf(os.Stderr, "loa: %s\n", snap.Unavailable)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tSTATUS\tWORKSPACE\tTRIES\tSESSION\tTITLE"); err != nil {
		return err
	}
	for _, row := range snap.Rows {
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%d/%d\t%s\t%s\n",
			row.Task.ID, row.Task.Status, workspaceOf(&row.Task),
			row.Task.Attempts, row.Task.MaxAttempts, row.Session(), row.Task.Title); err != nil {
			return err
		}
	}
	for _, o := range snap.Orphans {
		if _, err := fmt.Fprintf(w, "-\torphan\t%s\t-\t%s\t(no task)\n",
			o.Label, o.WorkspaceID); err != nil {
			return err
		}
	}
	return w.Flush()
}

// cmdTriage runs one read-only agent against the queue and keeps the proposal it
// writes. It never applies it: a person does that with `loa import`.
func cmdTriage(ctx context.Context, args []string) error {
	var (
		timeout time.Duration
		model   string
		kind    string
		outDir  string
	)
	s, positional, err := open(ctx, "triage", args, func(fs *flag.FlagSet) {
		fs.DurationVar(&timeout, "timeout", 10*time.Minute, "limit for the triage agent")
		fs.StringVar(&model, "model", "", "model passed to the agent")
		fs.StringVar(&kind, "kind", "pi", "agent kind Herdr starts")
		fs.StringVar(&outDir, "out", "", "where proposals are kept (default $XDG_STATE_HOME/loa/proposals)")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}
	if !herdr.InSession() {
		return errors.New("triage: not inside a Herdr pane, so there is no session to run an agent in")
	}
	if outDir == "" {
		outDir = defaultProposalDir()
	}

	client := &herdr.Client{}
	if _, err := client.CheckStatus(ctx); err != nil {
		return err
	}

	// The agent reads the queue from a file, so its context is one document rather
	// than whatever the model would have inferred from a command.
	tasks, err := s.List(ctx)
	if err != nil {
		return err
	}
	deps := make(map[int64][]store.Dep, len(tasks))
	for _, task := range tasks {
		got, err := s.Deps(ctx, task.ID)
		if err != nil {
			return err
		}
		deps[task.ID] = got
	}
	queue := triage.Build(time.Now(), tasks, deps)

	scratch, err := os.MkdirTemp("", "loa-triage-")
	if err != nil {
		return fmt.Errorf("triage: scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	queuePath, err := triage.Context(scratch, queue)
	if err != nil {
		return err
	}

	workspace, err := client.CreateWorkspace(ctx, scratch, "loa triage")
	if err != nil {
		return fmt.Errorf("triage: %w", err)
	}
	// The scratch directory outlives the workspace, so a failed proposal can still
	// be read; the pane is what needs closing.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := client.CloseWorkspace(cleanup, workspace.ID); err != nil {
			fmt.Fprintf(os.Stderr, "triage: close workspace %s: %v\n", workspace.ID, err)
		}
	}()

	agentName := "loa-triage"
	agentArgs := []string{"--no-session", "--tools", "read,grep,find,ls,write"}
	if model != "" {
		agentArgs = append(agentArgs, "--model", model)
	}
	if err := client.StartAgent(ctx, herdr.StartAgentRequest{
		Name: agentName, Kind: kind, PaneID: workspace.PaneID,
		Args: agentArgs, Timeout: time.Minute,
	}); err != nil {
		return fmt.Errorf("triage: %w", err)
	}

	if err := client.Prompt(ctx, agentName, triage.Prompt(queuePath), timeout); err != nil {
		if herdr.CodeOf(err) == "agent_blocked" {
			return fmt.Errorf("triage: the agent is waiting for input in workspace %s (pane %s); answer it there, then run triage again",
				workspace.ID, workspace.PaneID)
		}
		return fmt.Errorf("triage: %w", err)
	}

	doc, err := triage.Read(scratch)
	if err != nil {
		return fmt.Errorf("%w (the agent's scratch directory is %s)", err, scratch)
	}
	path, err := triage.Save(outDir, doc, time.Now())
	if err != nil {
		return err
	}

	fmt.Println(path)
	fmt.Fprintf(os.Stderr, "triage: %d proposed task(s), %d priority change(s), %d note(s)\n",
		len(doc.Tasks), len(doc.Reprioritise), len(doc.Notes))
	for _, note := range doc.Notes {
		fmt.Fprintf(os.Stderr, "note: %s\n", note)
	}
	if len(doc.Tasks) > 0 || len(doc.Reprioritise) > 0 {
		fmt.Fprintf(os.Stderr, "apply with: loa import %s\n", path)
	}
	return nil
}

// defaultStateDir is where loa keeps what outlives one command.
func defaultStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "loa")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "loa")
	}
	return filepath.Join(home, ".local", "state", "loa")
}

// defaultProposalDir is where triage keeps what it proposes.
func defaultProposalDir() string {
	return filepath.Join(defaultStateDir(), "proposals")
}

// cmdImport applies a proposal. It is the only door through which a
// machine-authored file becomes queue changes, and it opens whole or not at all.
func cmdImport(ctx context.Context, args []string) error {
	var dryRun bool
	s, positional, err := open(ctx, "import", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&dryRun, "dry-run", false, "validate and print, creating nothing")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 1 {
		return fmt.Errorf("import: at most one file")
	}
	source := "-"
	if len(positional) == 1 {
		source = positional[0]
	}

	var input io.Reader = os.Stdin
	if source != "-" {
		// The path is the caller's own argument, like -db.
		file, err := os.Open(source) //nolint:gosec // G703: the file to import is named by the user
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		defer func() { _ = file.Close() }()
		input = file
	}

	doc, err := taskspec.Parse(input)
	if err != nil {
		return err
	}
	applied, err := taskspec.Validate(doc)
	if err != nil {
		return err
	}

	items := make([]store.BatchItem, len(applied.Tasks))
	for i, task := range applied.Tasks {
		items[i] = store.BatchItem{
			NewTask: store.NewTask{
				Title: task.Title, Body: task.Body, Workspace: task.Workspace,
				Priority: task.Priority, MaxTries: task.MaxAttempts, Scopes: task.Scopes,
			},
			Deps:  task.Deps,
			After: applied.After[i],
		}
	}
	priorities := make([]store.Priority, len(applied.Reprioritise))
	for i, change := range applied.Reprioritise {
		priorities[i] = store.Priority{ID: change.ID, Priority: change.Priority}
	}

	if dryRun {
		out := struct {
			Tasks        []taskspec.Task     `json:"tasks"`
			Reprioritise []taskspec.Priority `json:"reprioritise"`
			Notes        []string            `json:"notes,omitempty"`
		}{doc.Tasks, doc.Reprioritise, doc.Notes}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "import: would create %d task(s) and change %d priority(ies)\n",
			len(items), len(priorities))
		return nil
	}

	ids, err := s.Apply(ctx, items, priorities)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	fmt.Fprintf(os.Stderr, "import: created %d task(s), changed %d priority(ies)\n", len(ids), len(priorities))
	for _, note := range applied.Notes {
		fmt.Fprintf(os.Stderr, "note: %s\n", note)
	}
	return nil
}

// cmdReport is the page a person reads after a day away: deterministic, no model.
func cmdReport(ctx context.Context, args []string) error {
	var (
		asJSON bool
		since  string
	)
	s, positional, err := open(ctx, "report", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "emit the report as JSON")
		fs.StringVar(&since, "since", "24h", "window: a duration like 24h, or a date like 2026-11-18")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}

	now := time.Now()
	start, err := parseSince(since, now)
	if err != nil {
		return err
	}

	snap, err := live.Read(ctx, s, &herdr.Client{})
	if err != nil {
		return err
	}
	r := report.Build(snap, start, now)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Print(r.Text())
	return nil
}

// parseSince accepts a duration ("24h") or a date ("2026-11-18").
func parseSince(value string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(value); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("-since %q: the window must be positive", value)
		}
		return now.Add(-d), nil
	}
	if day, err := time.Parse("2006-01-02", value); err == nil {
		return day, nil
	}
	return time.Time{}, fmt.Errorf("-since %q: use a duration like 24h, or a date like 2026-11-18", value)
}

func cmdLease(ctx context.Context, args []string) error {
	var (
		owner string
		ttl   time.Duration
	)
	s, _, err := open(ctx, "lease", args, func(fs *flag.FlagSet) {
		fs.StringVar(&owner, "owner", defaultOwner(), "who holds the lease")
		fs.DurationVar(&ttl, "ttl", time.Hour, "when the lease lapses if not finished")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	task, err := s.Lease(ctx, owner, ttl, time.Now())
	if err != nil {
		return err
	}

	// null means nothing is runnable right now; the caller can always parse.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(task)
}

func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "runner"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// workspaceOf keeps an unset workspace visible rather than printing nothing.
func workspaceOf(task *store.Task) string {
	if task.Workspace == "" {
		return "(unset)"
	}
	return task.Workspace
}

// allowlist is the tool set a worker gets. It is named here rather than
// inherited from the human's settings, so a change there cannot silently widen
// what an autonomous agent may do.
var allowlist = []string{"read", "edit", "write", "bash", "grep", "find", "ls"}

// runFlags is what both `run` and `dash` need to start a run of the queue.
type runFlags struct {
	workers  int
	maxTasks int
	base     string
	timeout  time.Duration
	ttl      time.Duration
	owner    string
	model    string
	kind     string
}

func registerRunFlags(fs *flag.FlagSet, f *runFlags) {
	fs.IntVar(&f.workers, "workers", 2, "tasks run concurrently")
	fs.IntVar(&f.maxTasks, "max", 5, "tasks this run may start; the budget")
	fs.StringVar(&f.base, "base", "HEAD", "commit or ref worktrees branch from")
	fs.DurationVar(&f.timeout, "timeout", 45*time.Minute, "limit for one agent run")
	fs.DurationVar(&f.ttl, "ttl", 2*time.Hour, "lease lifetime; must exceed -timeout")
	fs.StringVar(&f.owner, "owner", defaultOwner(), "who holds the leases")
	fs.StringVar(&f.model, "model", "", "model passed to the agent")
	fs.StringVar(&f.kind, "kind", "pi", "agent kind Herdr starts")
}

// buildRunner assembles the runner both commands use. One binary, one way to
// work the queue. logw is separate from the process's stderr because `dash`
// owns the terminal it runs on: its run log goes to a file.
func buildRunner(ctx context.Context, s *store.Store, f runFlags, logw io.Writer) (*runner.Runner, error) {
	if !herdr.InSession() {
		return nil, errors.New("not inside a Herdr pane (HERDR_ENV is not 1), so there is no session to work in")
	}
	client := &herdr.Client{}
	if _, err := client.CheckStatus(ctx); err != nil {
		return nil, err
	}

	agentArgs := []string{"--no-session", "--tools", strings.Join(allowlist, ",")}
	if f.model != "" {
		agentArgs = append(agentArgs, "--model", f.model)
	}

	return runner.New(runner.Config{
		Store: s,
		Backend: &runner.Herdr{
			Client:    client,
			AgentKind: f.kind,
			AgentArgs: agentArgs,
		},
		Base:        f.base,
		Workers:     f.workers,
		MaxTasks:    f.maxTasks,
		LeaseTTL:    f.ttl,
		TaskTimeout: f.timeout,
		Logger:      log.New(logw, "loa: ", log.LstdFlags),
	})
}

func cmdRun(ctx context.Context, args []string) error {
	var flags runFlags
	var asJSON bool
	s, positional, err := open(ctx, "run", args, func(fs *flag.FlagSet) {
		registerRunFlags(fs, &flags)
		fs.BoolVar(&asJSON, "json", false, "emit the run outcome as JSON")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}

	r, err := buildRunner(ctx, s, flags, os.Stderr)
	if err != nil {
		return err
	}
	stats, runErr := r.Run(ctx)

	// A run that never started has no row to report; the error says why.
	if asJSON && stats.Run.ID != 0 {
		if err := printRunJSON(os.Stdout, stats); err != nil {
			return err
		}
	}
	if runErr != nil {
		return runErr
	}

	fmt.Fprintf(os.Stderr, "loa: %d task(s): %d done, %d failed\n", stats.Tasks, stats.Completed, stats.Failed)
	return nil
}

type runJSON struct {
	ID        int64  `json:"id"`
	Owner     string `json:"owner"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	Workers   int    `json:"workers"`
	MaxTasks  int    `json:"max_tasks"`
	Done      int    `json:"done"`
	Failed    int    `json:"failed"`
}

type runTaskJSON struct {
	ID          int64  `json:"id"`
	Status      string `json:"status"`
	Branch      string `json:"branch"`
	WorkspaceID string `json:"workspace_id"`
	PaneID      string `json:"pane_id"`
	Blocked     bool   `json:"blocked"`
}

type runReportJSON struct {
	Run   runJSON       `json:"run"`
	Tasks []runTaskJSON `json:"tasks"`
}

func newRunJSON(run store.Run) runJSON {
	return runJSON{
		ID: run.ID, Owner: run.Owner, StartedAt: run.StartedAt, EndedAt: run.EndedAt,
		Workers: run.Workers, MaxTasks: run.MaxTasks, Done: run.Done, Failed: run.Failed,
	}
}

// printRunJSON is the machine-readable half of `loa run`: one document, and
// nothing else on stdout. The runner's log stays on stderr.
func printRunJSON(w io.Writer, stats runner.Stats) error {
	out := runReportJSON{
		Run:   newRunJSON(stats.Run),
		Tasks: make([]runTaskJSON, 0, len(stats.Outcomes)),
	}
	for _, task := range stats.Outcomes {
		status := "failed"
		switch {
		case task.Done:
			status = "done"
		case task.Blocked:
			status = "blocked"
		}
		out.Tasks = append(out.Tasks, runTaskJSON{
			ID: task.ID, Status: status, Branch: task.Branch,
			WorkspaceID: task.WorkspaceID, PaneID: task.PaneID, Blocked: task.Blocked,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// cmdRuns lists every run, newest first, or settles one with -close. The open
// row is marked rather than hidden: a run left active by a crash is exactly
// what a person needs to see, and -close is the way out of it.
func cmdRuns(ctx context.Context, args []string) error {
	var (
		asJSON  bool
		limit   int
		closeID int64
	)
	s, positional, err := open(ctx, "runs", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "emit the runs as JSON")
		fs.IntVar(&limit, "limit", 20, "how many runs to show")
		fs.Int64Var(&closeID, "close", 0, "settle the run with this id")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}
	if closeID < 0 {
		return fmt.Errorf("-close %d: run id must be positive", closeID)
	}

	if closeID > 0 {
		if err := s.SettleRun(ctx, closeID); err != nil {
			return err
		}
		run, err := s.GetRun(ctx, closeID)
		if err != nil {
			return err
		}
		return printRuns([]store.Run{*run}, asJSON)
	}

	runs, err := s.Runs(ctx, limit)
	if err != nil {
		return err
	}
	return printRuns(runs, asJSON)
}

// printRuns writes the run record as one JSON document, or as the table.
func printRuns(runs []store.Run, asJSON bool) error {
	if asJSON {
		out := struct {
			Runs []runJSON `json:"runs"`
		}{Runs: make([]runJSON, 0, len(runs))}
		for _, run := range runs {
			out.Runs = append(out.Runs, newRunJSON(run))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tOWNER\tSTARTED\tENDED\tDONE\tFAILED\tBUDGET"); err != nil {
		return err
	}
	for _, run := range runs {
		ended := run.EndedAt
		if run.Active() {
			ended = "- [active]"
		}
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%d\t%d/%d\n",
			run.ID, run.Owner, run.StartedAt, ended, run.Done, run.Failed,
			run.Workers, run.MaxTasks); err != nil {
			return err
		}
	}
	return w.Flush()
}

// controls is what the dashboard tab may do. Each method is a call that exists
// elsewhere; the tab adds no policy of its own.
type controls struct {
	client *herdr.Client
	store  *store.Store
	runFn  func(ctx context.Context) error
}

func (c controls) Focus(ctx context.Context, row live.Row) error {
	if row.AgentName != "" {
		return c.client.FocusAgent(ctx, row.AgentName)
	}
	if row.WorkspaceID != "" {
		return c.client.FocusWorkspace(ctx, row.WorkspaceID)
	}
	return errors.New("nothing to focus: this task has never been dispatched")
}

func (c controls) Interrupt(ctx context.Context, row live.Row) error {
	return c.client.SendKeys(ctx, row.AgentName, "ctrl+c")
}

func (c controls) RemoveWorkspace(ctx context.Context, row live.Row) error {
	return c.client.RemoveWorktree(ctx, row.WorkspaceID)
}

func (c controls) Unblock(ctx context.Context, taskID int64) error {
	return c.store.Unblock(ctx, taskID)
}

func (c controls) Resolve(ctx context.Context, taskID int64, done bool, result string) error {
	return c.store.Resolve(ctx, taskID, done, result)
}

func (c controls) Run(ctx context.Context) error { return c.runFn(ctx) }

func cmdDash(ctx context.Context, args []string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("dash needs a terminal")
	}

	var flags runFlags
	s, positional, err := open(ctx, "dash", args, func(fs *flag.FlagSet) { registerRunFlags(fs, &flags) })
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}

	// The tab owns the terminal. A run started from it must not write the runner's
	// log over the view, so the log goes to a file and the path is printed once.
	if err := os.MkdirAll(defaultStateDir(), 0o700); err != nil {
		return fmt.Errorf("dash: state dir: %w", err)
	}
	logPath := filepath.Join(defaultStateDir(), "dash.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: the state dir the runner already owns
	if err != nil {
		return fmt.Errorf("dash: run log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	client := &herdr.Client{}
	r, err := buildRunner(ctx, s, flags, logFile)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loa: run log: %s\n", logPath)

	c := controls{client: client, store: s}
	c.runFn = func(ctx context.Context) error {
		// Progress is the tab's job, not a log line's: the summary lands in the
		// message line, and the details are already in the log file.
		stats, err := r.Run(ctx)
		if err != nil {
			return err
		}
		// A failed write here is a full disk; the run still happened.
		_, _ = fmt.Fprintf(logFile, "loa: %d task(s): %d done, %d failed\n", stats.Tasks, stats.Completed, stats.Failed)
		return nil
	}

	return dash.Run(ctx, dash.New(s, client, c), os.Stdin, os.Stdout)
}

func cmdFinish(ctx context.Context, args []string, done bool) error {
	var (
		branch string
		result string
	)
	name := "fail"
	if done {
		name = "done"
	}
	s, positional, err := open(ctx, name, args, func(fs *flag.FlagSet) {
		fs.StringVar(&branch, "branch", "", "branch holding the work")
		fs.StringVar(&result, "result", "", "outcome or failure reason")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	id, err := taskID(name, positional)
	if err != nil {
		return err
	}

	if done {
		return s.Complete(ctx, id, branch, result)
	}
	return s.Fail(ctx, id, result)
}

// cmdUnblock returns a blocked task to the queue without waiting for a run.
func cmdUnblock(ctx context.Context, args []string) error {
	s, positional, err := open(ctx, "unblock", args, func(*flag.FlagSet) {})
	if err != nil {
		return err
	}
	defer s.Close()

	id, err := taskID("unblock", positional)
	if err != nil {
		return err
	}
	return s.Unblock(ctx, id)
}

// cmdResolve settles a task a human had to answer: done by default, or blocked
// with -fail.
func cmdResolve(ctx context.Context, args []string) error {
	var (
		fail   bool
		result string
	)
	s, positional, err := open(ctx, "resolve", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&fail, "fail", false, "mark the task blocked instead of done")
		fs.StringVar(&result, "result", "", "outcome or failure reason")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	id, err := taskID("resolve", positional)
	if err != nil {
		return err
	}
	return s.Resolve(ctx, id, !fail, result)
}

// taskID reads the one positional argument shared by the commands that act on
// a task.
func taskID(name string, positional []string) (int64, error) {
	if len(positional) != 1 {
		return 0, fmt.Errorf("%s: need exactly one task id", name)
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return id, nil
}
