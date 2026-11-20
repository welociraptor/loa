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
// The run flags are the one place the daemon's default budget lives; `loa run`
// and the tab may narrow it for one run, but a run request cannot change it.
func cmdDaemon(ctx context.Context, args []string) error {
	var status bool
	var flags runFlags
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "path to the task database")
	fs.BoolVar(&status, "status", false, "report the running daemon and exit")
	registerRunFlags(fs, &flags)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return fmt.Errorf("daemon: unexpected arguments: %v", positional)
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
		Runner: newRunnerConfig(s, backend, flags, os.Stderr),
	})
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
