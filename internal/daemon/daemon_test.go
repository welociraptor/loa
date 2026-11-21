package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type running struct {
	dir    string
	sock   string
	cancel context.CancelFunc
	errCh  chan error
}

func start(t *testing.T, cfg Config) *running {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	ctx, cancel := context.WithCancel(context.Background())
	r := &running{
		dir:    dir,
		sock:   filepath.Join(dir, "loa", "sock"),
		cancel: cancel,
		errCh:  make(chan error, 1),
	}
	go func() { r.errCh <- Run(ctx, cfg) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-r.errCh:
			t.Fatalf("daemon stopped before listening: %v", err)
		default:
		}
		if _, err := os.Stat(r.sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %s was not created", r.sock)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.errCh:
		if err != nil {
			t.Fatalf("daemon stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func dial(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestForeignUIDIsRefusedThroughTheInjectedCheck(t *testing.T) {
	checked := make(chan struct{}, 1)
	var logs lockedBuffer
	r := start(t, Config{
		Logger: log.New(&logs, "", 0),
		PeerCheck: func(*net.UnixConn) error {
			checked <- struct{}{}
			return errors.New("foreign uid 4242")
		},
	})
	defer r.stop(t)

	conn := dial(t, r.sock)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err == nil || n != 0 {
		t.Fatalf("foreign connection read %d bytes, err %v; want an immediate close", n, err)
	}
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("the injected peer check was not called")
	}
	if !strings.Contains(logs.String(), "foreign uid 4242") {
		t.Errorf("logs = %q, want the refusal", logs.String())
	}
}

func TestSecondDaemonRefusesWhileTheFirstHoldsTheLock(t *testing.T) {
	first := start(t, Config{})
	defer first.stop(t)

	err := Run(context.Background(), Config{})
	if err == nil {
		t.Fatal("the second daemon started")
	}
	if !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("second daemon = %v, want the holder's pid", err)
	}
}

func TestWrongVersionIsRefusedAndCloses(t *testing.T) {
	r := start(t, Config{})
	defer r.stop(t)

	conn := dial(t, r.sock)
	if _, err := conn.Write([]byte(`{"v":2,"do":"ping"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if resp.OK || resp.V != Protocol {
		t.Errorf("refusal = %+v, want ok false and protocol %d", resp, Protocol)
	}
	if want := "protocol 2 is newer than this daemon (1)"; resp.Error != want {
		t.Errorf("error = %q, want %q", resp.Error, want)
	}

	// The refusal is followed by a close, not by another answer on the socket.
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("the connection stayed open after the version refusal")
	}
}

func TestOversizedRequestIsRefusedAndTheDaemonKeepsServing(t *testing.T) {
	r := start(t, Config{})
	defer r.stop(t)

	conn := dial(t, r.sock)
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := conn.Write([]byte(strings.Repeat("x", maxRequest+1) + "\n")); err != nil {
		t.Fatalf("write oversized request: %v", err)
	}

	var refusal Response
	if err := json.NewDecoder(conn).Decode(&refusal); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if refusal.OK || refusal.V != Protocol || refusal.Error != "request is too large" {
		t.Errorf("refusal = %+v, want an oversized-request refusal", refusal)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("the connection stayed open after the oversized request")
	}

	// The refusal is per connection: another client still gets an answer.
	other := dial(t, r.sock)
	if err := other.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := other.Write([]byte(`{"v":1,"do":"ping"}` + "\n")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	var pong Response
	if err := json.NewDecoder(other).Decode(&pong); err != nil {
		t.Fatalf("decode ping: %v", err)
	}
	if !pong.OK || !pong.Pong {
		t.Errorf("ping = %+v, want the daemon still serving", pong)
	}
}

func TestStatusShowsNoRunAndPauseSurvives(t *testing.T) {
	r := start(t, Config{})
	defer r.stop(t)

	client := &Client{Path: r.sock}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.PID != os.Getpid() || status.Protocol != Protocol || status.Paused || status.Run != nil {
		t.Errorf("status = %+v", status)
	}

	for _, verb := range []string{"pause", "resume"} {
		resp, err := client.Do(context.Background(), verb)
		if err != nil || !resp.OK {
			t.Fatalf("%s = %+v, %v", verb, resp, err)
		}
		status, err = client.Status(context.Background())
		if err != nil {
			t.Fatalf("status after %s: %v", verb, err)
		}
		if want := verb == "pause"; status.Paused != want {
			t.Errorf("paused after %s = %t, want %t", verb, status.Paused, want)
		}
	}
}

func TestStatusReportsTheDaemonDatabase(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "tasks.db")
	r := start(t, Config{DBPath: want})
	defer r.stop(t)

	status, err := (&Client{Path: r.sock}).Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.DB != want {
		t.Errorf("status.DB = %q, want %q", status.DB, want)
	}
}

func TestRunRefusesWithoutARuntimeDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	err := Run(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Fatalf("err = %v, want a refusal naming XDG_RUNTIME_DIR", err)
	}
}

func TestStaleSocketIsUntouchedWhileTheLockIsHeld(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	loaDir := filepath.Join(dir, "loa")
	if err := os.MkdirAll(loaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lock, err := acquireLock(filepath.Join(loaDir, lockName))
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer func() { _ = lock.Close() }()

	stale := filepath.Join(loaDir, socketName)
	if err := os.WriteFile(stale, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}

	err = Run(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Run = %v, want a lock refusal", err)
	}
	// The lock holder still owns the path; a refused daemon must not clear it.
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("the refused daemon removed the socket: %v", err)
	}
}
