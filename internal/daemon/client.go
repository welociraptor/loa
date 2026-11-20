package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"loa/internal/runner"
)

// SocketPath is where the daemon listens. XDG_RUNTIME_DIR is required: falling
// back to /tmp would put a control socket in a world-writable directory.
func SocketPath() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		return "", errors.New("daemon: XDG_RUNTIME_DIR is not set; refusing to put a control socket in a world-writable directory")
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("daemon: XDG_RUNTIME_DIR %q is not absolute", base)
	}
	return filepath.Join(base, "loa", "sock"), nil
}

// Client is one connection to the daemon. Every call is a fresh connection so
// a dead daemon cannot leave a half-open client holding a stale request.
type Client struct {
	Path string
}

// Do sends one request and reads one response. A refusal is a response, not a
// transport error; the caller decides what to do with it.
func (c *Client) Do(ctx context.Context, verb string) (*Response, error) {
	return c.send(ctx, Request{V: Protocol, Do: verb})
}

// Run asks the daemon to start a run and waits for its outcome. The wait is on
// the daemon's run, not on this connection: closing the client does not stop
// the work.
func (c *Client) Run(ctx context.Context, opts RunOptions) (runner.Stats, error) {
	resp, err := c.send(ctx, Request{V: Protocol, Do: "run", Run: &opts})
	if err != nil {
		return runner.Stats{}, err
	}
	var stats runner.Stats
	if resp.Run != nil {
		stats = resp.Run.Stats()
	}
	if !resp.OK {
		return stats, errors.New("daemon: " + resp.Error)
	}
	if resp.Run == nil {
		return stats, errors.New("daemon: run response carried no result")
	}
	return stats, nil
}

// Stop asks the daemon to interrupt the run in flight. It is a no-op when no
// run is active, so a tab can always send it.
func (c *Client) Stop(ctx context.Context) error {
	return c.control(ctx, "stop")
}

// Pause stops automatic starts without touching the run in flight. It lives in
// the daemon's memory, so a restart starts with a clean, unpaused daemon.
func (c *Client) Pause(ctx context.Context) error {
	return c.control(ctx, "pause")
}

// Resume allows automatic starts again.
func (c *Client) Resume(ctx context.Context) error {
	return c.control(ctx, "resume")
}

func (c *Client) control(ctx context.Context, verb string) error {
	resp, err := c.Do(ctx, verb)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New("daemon: " + resp.Error)
	}
	return nil
}

func (c *Client) send(ctx context.Context, req Request) (*Response, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, fmt.Errorf("daemon: connect to %s: %w", c.Path, err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("daemon: set deadline: %w", err)
		}
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("daemon: send %q: %w", req.Do, err)
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("daemon: read %q response: %w", req.Do, err)
	}
	if resp.V != Protocol {
		return nil, fmt.Errorf("daemon: speaks protocol %d; this client speaks %d", resp.V, Protocol)
	}
	return &resp, nil
}

// Status asks for the daemon's status and turns a refusal into an error.
func (c *Client) Status(ctx context.Context) (Status, error) {
	resp, err := c.Do(ctx, "status")
	if err != nil {
		return Status{}, err
	}
	if !resp.OK {
		return Status{}, errors.New("daemon: " + resp.Error)
	}
	if resp.Status == nil {
		return Status{}, errors.New("daemon: status response carried no status")
	}
	return *resp.Status, nil
}
