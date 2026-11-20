// Package daemon is the control plane for a loa supervisor: an NDJSON protocol
// over a Unix socket, the client that speaks it, and the server that answers.
package daemon

// Protocol is the version of the line protocol this package speaks. A request
// from another version is refused rather than guessed at.
const Protocol = 1

// Request is one line from a client. Every request names its protocol version
// so an old daemon can refuse a newer client before it touches anything.
type Request struct {
	V  int    `json:"v"`
	Do string `json:"do"`
}

// Response is one line back. V and OK are on every response, including the
// refusal of a version this daemon does not speak.
type Response struct {
	V      int     `json:"v"`
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Pong   bool    `json:"pong,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// Status is what the daemon knows about itself. Run is nil until a scheduler
// exists; the field is already here so the shape does not change when it does.
type Status struct {
	PID      int        `json:"pid"`
	Uptime   string     `json:"uptime"`
	Protocol int        `json:"protocol"`
	Paused   bool       `json:"paused"`
	Run      *ActiveRun `json:"run,omitempty"`
}

// ActiveRun is the active run's identity, enough to tell two daemons apart in a
// status line. The full record stays in the database.
type ActiveRun struct {
	ID        int64  `json:"id"`
	Owner     string `json:"owner,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}
