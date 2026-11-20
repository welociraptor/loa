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
	fs.IntVar(&daily, "daily", 12, "tasks auto-started runs may settle per day; 0 means no daily cap")
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

// cmdPause and cmdResume are the brake. They act on this daemon only: the
// pause is memory, not a file, so a restart resumes.
func cmdPause(ctx context.Context, args []string) error {
	return cmdDaemonControl(ctx, "pause", args)
}

func cmdResume(ctx context.Context, args []string) error {
	return cmdDaemonControl(ctx, "resume", args)
}

func cmdDaemonControl(ctx context.Context, verb string, args []string) error {
	fs := flag.NewFlagSet(verb, flag.ExitOnError)
	fs.String("db", defaultDB(), "path to the task database (accepted for consistency; the daemon chooses it)")
	positional, err := parseFlags(fs, args)
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
	if verb == "pause" {
		err = client.Pause(ctx)
	} else {
		err = client.Resume(ctx)
	}
	if err != nil {
		return err
	}
	if verb == "pause" {
		fmt.Println("auto runs paused in this daemon; a restart resumes")
	} else {
		fmt.Println("auto runs resumed")
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
	fmt.Printf("pid %d, up %s, protocol %d, paused %t, %s\n",
		status.PID, status.Uptime, status.Protocol, status.Paused, run)
	return nil
}
