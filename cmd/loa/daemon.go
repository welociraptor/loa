package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"loa/internal/daemon"
	"loa/internal/herdr"
	"loa/internal/store"
)

// cmdDaemon serves the control socket, or asks a running one for its status.
// The run flags are the daemon's policy: its budget and its check. `loa run`
// and the tab may narrow that budget for one run, and may choose the base,
// prompts, model and kind, but a request cannot widen the budget, lengthen a
// timeout or replace the daemon's check.
func cmdDaemon(ctx context.Context, args []string) error {
	var status, auto bool
	var (
		idle     time.Duration
		interval time.Duration
		daily    int
	)
	var flags runFlags
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "path to the task database")
	fs.BoolVar(&status, "status", false, "report the running daemon and exit")
	fs.BoolVar(&auto, "auto", false, "start runs when a task is claimable; off by default")
	fs.DurationVar(&idle, "idle", 5*time.Minute, "wait this long after the last settled run before auto-starting another")
	fs.DurationVar(&interval, "interval", time.Minute, "how often auto checks the queue; also the floor for an empty queue")
	fs.IntVar(&daily, "daily", 12, "tasks settled per day, counted from every run; 0 means no daily cap")
	registerRunFlags(fs, &flags)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return fmt.Errorf("daemon: unexpected arguments: %v", positional)
	}
	if interval <= 0 {
		return fmt.Errorf("daemon: -interval %s must be positive", interval)
	}
	if idle < 0 {
		return fmt.Errorf("daemon: -idle %s must not be negative", idle)
	}
	if daily < 0 {
		return fmt.Errorf("daemon: -daily %d must not be negative", daily)
	}

	if status {
		if flagWasSet(fs, "db") {
			return fmt.Errorf("daemon -status: the daemon decides the database; drop -db")
		}
		return daemonStatus(ctx)
	}

	s, err := store.Open(ctx, *db)
	if err != nil {
		return err
	}
	defer s.Close()

	backend, err := newRunnerBackend(flags)
	if err != nil {
		return err
	}

	// The daemon outlives the shell but not a supervisor's SIGTERM. Catching both
	// signals here is what makes the socket cleanup and the run's cleanup below
	// happen on the way out.
	sigctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	return daemon.Run(sigctx, daemon.Config{
		Logger: log.New(os.Stderr, "loa: ", log.LstdFlags),
		// The daemon is the authority on the queue, so its status names the
		// database a client would have to agree with.
		DBPath: *db,
		Probe: func(ctx context.Context) error {
			_, err := (&herdr.Client{}).CheckStatus(ctx)
			return err
		},
		Runner:   newRunnerConfig(s, backend, flags, os.Stderr),
		Auto:     auto,
		Idle:     idle,
		Daily:    daily,
		Interval: interval,
	})
}

// cmdPause, cmdResume and cmdStop are control calls. They act on this daemon
// only: the pause is memory, not a file, so a restart resumes.
func cmdPause(ctx context.Context, args []string) error {
	return cmdDaemonControl(ctx, "pause", args)
}

func cmdResume(ctx context.Context, args []string) error {
	return cmdDaemonControl(ctx, "resume", args)
}

func cmdStop(ctx context.Context, args []string) error {
	return cmdDaemonControl(ctx, "stop", args)
}

func cmdDaemonControl(ctx context.Context, verb string, args []string) error {
	// No -db: these are client calls, and the daemon owns the queue.
	positional, _, err := parseClientFlags(verb, args, func(*flag.FlagSet) {})
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return fmt.Errorf("%s: unexpected arguments: %v", verb, positional)
	}

	path, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	client := &daemon.Client{Path: path}
	switch verb {
	case "pause":
		err = client.Pause(ctx)
	case "resume":
		err = client.Resume(ctx)
	case "stop":
		err = client.Stop(ctx)
	default:
		return fmt.Errorf("daemon control: unknown verb %q", verb)
	}
	if err != nil {
		return err
	}
	switch verb {
	case "pause":
		fmt.Println("auto runs paused in this daemon; a restart resumes")
	case "resume":
		fmt.Println("auto runs resumed")
	case "stop":
		fmt.Println("stop requested; the daemon will settle the run")
	}
	return nil
}

func daemonStatus(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	path, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	status, err := (&daemon.Client{Path: path}).Status(ctx)
	if err != nil {
		return err
	}

	run := "no active run"
	if status.Run != nil {
		run = fmt.Sprintf("active run %d", status.Run.ID)
		if status.Run.Owner != "" {
			run += " (" + status.Run.Owner + ")"
		}
	}
	db := status.DB
	if db == "" {
		db = "(not reported)"
	}
	fmt.Printf("pid %d, up %s, protocol %d, paused %t, db %s, %s\n",
		status.PID, status.Uptime, status.Protocol, status.Paused, db, run)
	return nil
}
