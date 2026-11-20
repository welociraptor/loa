package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	lockName     = "lock"
	socketName   = "sock"
	maxRequest   = 64 << 10
	probeTimeout = 10 * time.Second
)

// Config is what the process supplies to the daemon. PeerCheck is injectable so
// the refusal of a foreign uid can be tested without a second user.
type Config struct {
	Logger    *log.Logger
	PeerCheck func(*net.UnixConn) error
	Probe     func(context.Context) error
}

// Server answers requests. It owns only its own state: no runs, no database.
type Server struct {
	logger    *log.Logger
	peerCheck func(*net.UnixConn) error
	probeFn   func(context.Context) error
	started   time.Time

	mu     sync.Mutex
	paused bool
	run    *ActiveRun
}

// Run starts the daemon and serves until ctx is cancelled. The runtime lock is
// taken before the stale socket is touched, so a socket left by a dead process
// is reclaimed only by the process that now owns the instance.
func Run(ctx context.Context, cfg Config) error {
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemon: runtime dir %s: %w", dir, err)
	}
	// MkdirAll honours the umask; the control directory must not be narrower or
	// wider than the daemon's owner.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a runtime directory is 0700, not 0600
		return fmt.Errorf("daemon: runtime dir %s: %w", dir, err)
	}

	lockPath := filepath.Join(dir, lockName)
	lock, err := acquireLock(lockPath)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	sockPath := filepath.Join(dir, socketName)
	// The lock is ours, so anything at the socket path is debris.
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove stale socket %s: %w", sockPath, err)
	}

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("daemon: listen on %s: %w", sockPath, err)
	}
	// Remove the path ourselves while the lock is still held; letting Close do it
	// would make the order depend on the net package.
	ln.SetUnlinkOnClose(false)
	defer func() { _ = ln.Close() }()
	defer func() { _ = os.Remove(sockPath) }()

	if err := os.Chmod(sockPath, 0o600); err != nil {
		return fmt.Errorf("daemon: socket mode: %w", err)
	}

	srv := newServer(cfg)
	srv.logf("daemon: pid %d listening on %s (protocol %d)", os.Getpid(), sockPath, Protocol)

	return srv.serve(ctx, ln)
}

func runtimeDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		return "", errors.New("daemon: XDG_RUNTIME_DIR is not set; refusing to put a control socket in a world-writable directory")
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("daemon: XDG_RUNTIME_DIR %q is not absolute", base)
	}
	return filepath.Join(base, "loa"), nil
}

func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the path is built from XDG_RUNTIME_DIR
	if err != nil {
		return nil, fmt.Errorf("daemon: lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := lockHolder(path)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if holder != "" {
				return nil, fmt.Errorf("daemon: another daemon is already running (pid %s)", holder)
			}
			return nil, errors.New("daemon: another daemon holds the runtime lock")
		}
		return nil, fmt.Errorf("daemon: lock %s: %w", path, err)
	}

	// The lock file is the holder's name for the next process that asks.
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: record pid: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: record pid: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: record pid: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: record pid: %w", err)
	}
	return f, nil
}

func lockHolder(path string) string {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is built from XDG_RUNTIME_DIR
	if err != nil {
		return ""
	}
	field := strings.Fields(string(raw))
	if len(field) == 0 {
		return ""
	}
	if _, err := strconv.Atoi(field[0]); err != nil {
		return ""
	}
	return field[0]
}

func newServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{
		logger:    logger,
		peerCheck: cfg.PeerCheck,
		probeFn:   cfg.Probe,
		started:   time.Now(),
	}
}

func (s *Server) logf(format string, args ...any) {
	s.logger.Printf(format, args...)
}

func (s *Server) probe(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := s.probeFn(probeCtx); err != nil {
		s.logf("daemon: herdr status: %v", err)
		return
	}
	s.logf("daemon: herdr status: ok")
}

func (s *Server) serve(ctx context.Context, ln *net.UnixListener) error {
	if s.probeFn != nil {
		go s.probe(ctx)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-stop:
		}
	}()

	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("daemon: accept: %w", err)
		}
		if err := s.checkPeer(conn); err != nil {
			s.logf("daemon: refused connection from %s: %v", conn.RemoteAddr(), err)
			_ = conn.Close()
			continue
		}
		go s.serveConn(conn)
	}
}

func (s *Server) checkPeer(conn *net.UnixConn) error {
	if s.peerCheck != nil {
		return s.peerCheck(conn)
	}
	return checkPeerUID(conn)
}

func (s *Server) serveConn(conn *net.UnixConn) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			resp, closeAfter := s.handleLine(line)
			if err := json.NewEncoder(conn).Encode(resp); err != nil {
				return
			}
			if closeAfter {
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}

func (s *Server) handleLine(line []byte) (Response, bool) {
	if len(line) > maxRequest {
		return Response{V: Protocol, OK: false, Error: "request is too large"}, true
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return Response{V: Protocol, OK: false, Error: "invalid request: " + err.Error()}, true
	}
	if req.V != Protocol {
		return versionRefusal(req.V), true
	}

	switch req.Do {
	case "ping":
		return Response{V: Protocol, OK: true, Pong: true}, false
	case "status":
		status := s.status()
		return Response{V: Protocol, OK: true, Status: &status}, false
	case "stop":
		// No run exists yet; accepting the verb keeps the client contract stable.
		return Response{V: Protocol, OK: true}, false
	case "pause":
		s.mu.Lock()
		s.paused = true
		s.mu.Unlock()
		return Response{V: Protocol, OK: true}, false
	case "resume":
		s.mu.Lock()
		s.paused = false
		s.mu.Unlock()
		return Response{V: Protocol, OK: true}, false
	default:
		return Response{V: Protocol, OK: false, Error: fmt.Sprintf("unknown verb %q", req.Do)}, false
	}
}

func versionRefusal(version int) Response {
	message := fmt.Sprintf("protocol %d is not supported by this daemon (%d)", version, Protocol)
	if version > Protocol {
		message = fmt.Sprintf("protocol %d is newer than this daemon (%d)", version, Protocol)
	}
	return Response{V: Protocol, OK: false, Error: message}
}

func (s *Server) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		PID:      os.Getpid(),
		Uptime:   time.Since(s.started).Round(time.Second).String(),
		Protocol: Protocol,
		Paused:   s.paused,
		Run:      s.run,
	}
}
