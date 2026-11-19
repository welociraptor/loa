package dash

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	enterAltScreen = "\x1b[?1049h"
	leaveAltScreen = "\x1b[?1049l"
	hideCursor     = "\x1b[?25l"
	showCursor     = "\x1b[?25h"
	clearScreen    = "\x1b[H\x1b[2J"
)

// terminal is what the loop needs. The real one is a tty; tests record what
// was written so the restore path can be checked without one.
type terminal interface {
	open() (restore func() error, err error)
	size() (width, height int, err error)
	keys() <-chan Key
	resize() <-chan os.Signal
	write(string) error
}

// Run drives the model until it quits. It puts the terminal in raw mode, draws
// on the alt screen, and restores both from a defer and on SIGTERM.
func Run(ctx context.Context, m Model, in *os.File, out io.Writer) error {
	if !term.IsTerminal(int(in.Fd())) {
		return errors.New("dash: needs a terminal")
	}
	if f, ok := out.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
		return errors.New("dash: needs a terminal")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)

	return run(ctx, m, newStdTerminal(in, out), signals, m.refresh())
}

func run(ctx context.Context, m Model, t terminal, signals <-chan os.Signal, prime Cmd) (err error) {
	restore, err := t.open()
	if err != nil {
		return err
	}
	restored := false
	restoreTerm := func() {
		if restored {
			return
		}
		restored = true
		_ = t.write(showCursor + leaveAltScreen)
		if rerr := restore(); rerr != nil && err == nil {
			err = rerr
		}
	}
	defer restoreTerm()

	if err := t.write(enterAltScreen + hideCursor); err != nil {
		return err
	}

	width, height, err := t.size()
	if err != nil {
		return err
	}
	m, _ = m.Update(WindowSizeMsg{Width: width, Height: height})

	draw := func() error {
		return t.write(clearScreen + m.View())
	}
	if err := draw(); err != nil {
		return err
	}

	ticker := time.NewTicker(refreshEvery)
	defer ticker.Stop()

	keys, resizes := t.keys(), t.resize()
	results := make(chan Msg, 1)
	start := func(cmd Cmd) {
		if cmd == nil {
			return
		}
		go func() { results <- cmd() }()
	}

	// Prime the view instead of waiting for the first tick. Tests pass nil so
	// the loop itself can be checked without a store.
	start(prime)

	for {
		var cmd Cmd
		select {
		case <-ctx.Done():
			if m.cancel != nil {
				m.cancel()
			}
			return ctx.Err()
		case k, ok := <-keys:
			if !ok {
				return nil
			}
			m, cmd = m.Update(k)
		case <-ticker.C:
			m, cmd = m.Update(tickMsg(time.Now()))
		case msg := <-results:
			if _, ok := msg.(quitMsg); ok {
				return nil
			}
			m, cmd = m.Update(msg)
		case <-resizes:
			width, height, rerr := t.size()
			if rerr != nil {
				return rerr
			}
			m, cmd = m.Update(WindowSizeMsg{Width: width, Height: height})
		case <-signals:
			return nil
		}

		start(cmd)
		if err := draw(); err != nil {
			return err
		}
	}
}

// stdTerminal is os.Stdin/os.Stdout with x/term doing the raw-mode work.
type stdTerminal struct {
	in       *os.File
	out      io.Writer
	keyCh    chan Key
	resizeCh chan os.Signal
}

func newStdTerminal(in *os.File, out io.Writer) *stdTerminal {
	return &stdTerminal{
		in:       in,
		out:      out,
		keyCh:    make(chan Key),
		resizeCh: make(chan os.Signal, 1),
	}
}

func (t *stdTerminal) open() (func() error, error) {
	state, err := term.MakeRaw(int(t.in.Fd()))
	if err != nil {
		return nil, err
	}
	go decodeKeys(t.in, t.keyCh)
	signal.Notify(t.resizeCh, syscall.SIGWINCH)
	return func() error {
		signal.Stop(t.resizeCh)
		return term.Restore(int(t.in.Fd()), state)
	}, nil
}

func (t *stdTerminal) size() (int, int, error) {
	return term.GetSize(int(t.in.Fd()))
}

func (t *stdTerminal) keys() <-chan Key { return t.keyCh }

func (t *stdTerminal) resize() <-chan os.Signal { return t.resizeCh }

func (t *stdTerminal) write(s string) error {
	_, err := io.WriteString(t.out, s)
	return err
}

// decodeKeys turns the terminal's byte stream into Key values. The only
// awkward case is ESC: alone it is the Escape key, and followed by a CSI
// sequence it is an arrow. A short deadline separates the two, so a user
// pressing Escape does not wait for a sequence that will never arrive.
func decodeKeys(f *os.File, out chan<- Key) {
	defer close(out)
	br := bufio.NewReader(f)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return
		}
		switch b {
		case '\r', '\n':
			out <- Key{Type: KeyEnter}
		case 0x7f, 0x08:
			out <- Key{Type: KeyBackspace}
		case 0x03:
			out <- Key{Type: KeyCtrlC}
		case 0x1b:
			out <- readEscape(br, f)
		default:
			if err := br.UnreadByte(); err != nil {
				return
			}
			r, _, err := br.ReadRune()
			if err != nil {
				return
			}
			out <- Key{Type: KeyRunes, Runes: []rune{r}}
		}
	}
}

func readEscape(br *bufio.Reader, f *os.File) Key {
	_ = f.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
	defer func() { _ = f.SetReadDeadline(time.Time{}) }()

	b, err := br.ReadByte()
	if err != nil {
		return Key{Type: KeyEsc}
	}
	if b != '[' && b != 'O' {
		// Alt+key and friends: the Escape is real, and the byte belongs to the
		// next key press.
		_ = br.UnreadByte()
		return Key{Type: KeyEsc}
	}
	for {
		code, err := br.ReadByte()
		if err != nil {
			return Key{Type: KeyEsc}
		}
		switch code {
		case 'A':
			return Key{Type: KeyUp}
		case 'B':
			return Key{Type: KeyDown}
		case 'C':
			return Key{Type: KeyRight}
		case 'D':
			return Key{Type: KeyLeft}
		}
		if code >= 'A' && code <= 'Z' || code >= 'a' && code <= 'z' || code == '~' {
			// A sequence the tab does not bind: consume it whole and ignore it.
			return Key{Type: KeyRunes}
		}
	}
}
