// Command taskflow is the human- and script-facing edge of the task store.
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

	"taskflow/internal/runner"
	"taskflow/internal/store"
)

const usage = `taskflow — task queue for a multi-agent workflow

usage:
  taskflow add   -title TEXT [-body TEXT] [-priority N] [-max N] [-scope SCOPE]... [-dep ID]...
  taskflow list  [-status STATUS]...
  taskflow show  ID
  taskflow lease [-owner NAME] [-ttl 1h]
  taskflow done  ID [-branch BRANCH] [-result TEXT]
  taskflow fail  ID [-result TEXT]
  taskflow run   [-workers N] [-max N] [-repos DIR] [-timeout 45m]

all commands accept -db PATH (default $TASKFLOW_DB, else taskflow.db)`

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
	case "lease":
		err = cmdLease(ctx, os.Args[2:])
	case "done":
		err = cmdFinish(ctx, os.Args[2:], true)
	case "fail":
		err = cmdFinish(ctx, os.Args[2:], false)
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "taskflow: unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "taskflow: %v\n", err)
		os.Exit(1)
	}
}

func defaultDB() string {
	if p := os.Getenv("TASKFLOW_DB"); p != "" {
		return p
	}
	return "taskflow.db"
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
		title    string
		body     string
		priority int
		maxTries int
	)
	s, _, err := open(ctx, "add", args, func(fs *flag.FlagSet) {
		fs.StringVar(&title, "title", "", "task title")
		fs.StringVar(&body, "body", "", "task body handed to the agent")
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
		Title: title, Body: body, Priority: priority, MaxTries: maxTries,
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
	if _, err := fmt.Fprintln(w, "ID\tSTATUS\tPRI\tTRIES\tSCOPES\tTITLE"); err != nil {
		return err
	}
	for _, t := range tasks {
		scopes := "-"
		if len(t.Scopes) > 0 {
			scopes = strings.Join(t.Scopes, ",")
		}
		if _, err := fmt.Fprintf(w, "%d\t%s\t%d\t%d/%d\t%s\t%s\n",
			t.ID, t.Status, t.Priority, t.Attempts, t.MaxAttempts, scopes, t.Title); err != nil {
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

	if len(positional) != 1 {
		return errors.New("show: need exactly one task id")
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		return fmt.Errorf("show: %w", err)
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

func cmdRun(ctx context.Context, args []string) error {
	var (
		workers  int
		maxTasks int
		repos    string
		trees    string
		base     string
		timeout  time.Duration
		ttl      time.Duration
		owner    string
		model    string
		bin      string
	)
	s, positional, err := open(ctx, "run", args, func(fs *flag.FlagSet) {
		fs.IntVar(&workers, "workers", 2, "tasks run concurrently")
		fs.IntVar(&maxTasks, "max", 5, "tasks this run may start; the budget")
		fs.StringVar(&repos, "repos", "", "directory holding repo: checkouts (default ~/Repositories)")
		fs.StringVar(&trees, "worktrees", "", "where worktrees are created (default $XDG_STATE_HOME/taskflow/worktrees)")
		fs.StringVar(&base, "base", "HEAD", "commit or ref worktrees branch from")
		fs.DurationVar(&timeout, "timeout", 45*time.Minute, "limit for one agent run")
		fs.DurationVar(&ttl, "ttl", 2*time.Hour, "lease lifetime; must exceed -timeout")
		fs.StringVar(&owner, "owner", defaultOwner(), "who holds the leases")
		fs.StringVar(&model, "model", "", "model passed to pi")
		fs.StringVar(&bin, "pi", "pi", "pi binary")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 0 {
		return fmt.Errorf("unexpected arguments: %v", positional)
	}
	if repos == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot derive the repos dir: %w", err)
		}
		repos = filepath.Join(home, "Repositories")
	}

	r, err := runner.New(runner.Config{
		Store:       s,
		Agent:       runner.Pi{Bin: bin, Model: model},
		ReposDir:    repos,
		WorktreeDir: trees,
		Base:        base,
		Workers:     workers,
		MaxTasks:    maxTasks,
		LeaseTTL:    ttl,
		TaskTimeout: timeout,
		Logger:      log.New(os.Stderr, "taskflow: ", log.LstdFlags),
	})
	if err != nil {
		return err
	}

	stats, err := r.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "taskflow: %d task(s): %d done, %d failed\n", stats.Tasks, stats.Completed, stats.Failed)
	return nil
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

	if len(positional) != 1 {
		return fmt.Errorf("%s: need exactly one task id", name)
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	if done {
		return s.Complete(ctx, id, branch, result)
	}
	return s.Fail(ctx, id, result)
}
