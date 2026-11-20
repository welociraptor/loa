package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

	if err := json.NewEncoder(conn).Encode(Request{V: Protocol, Do: verb}); err != nil {
		return nil, fmt.Errorf("daemon: send %q: %w", verb, err)
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("daemon: read %q response: %w", verb, err)
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
