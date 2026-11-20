package dash

import (
	"bytes"
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
)

type fakeTerminal struct {
	out         bytes.Buffer
	width       int
	height      int
	keysCh      chan Key
	resizeCh    chan os.Signal
	panicOnSize bool
	restored    bool
}

func (f *fakeTerminal) open() (func() error, error) {
	return func() error {
		f.restored = true
		return nil
	}, nil
}

func (f *fakeTerminal) size() (int, int, error) {
	if f.panicOnSize {
		panic("size failed")
	}
	return f.width, f.height, nil
}

func (f *fakeTerminal) keys() <-chan Key { return f.keysCh }

func (f *fakeTerminal) resize() <-chan os.Signal { return f.resizeCh }

func (f *fakeTerminal) write(s string) error {
	_, err := f.out.WriteString(s)
	return err
}

func quitKey() chan Key {
	keys := make(chan Key, 1)
	keys <- Key{Type: KeyRunes, Runes: []rune("q")}
	return keys
}

func assertRestored(t *testing.T, f *fakeTerminal) {
	t.Helper()
	view := f.out.String()
	if !strings.Contains(view, enterAltScreen) || !strings.Contains(view, hideCursor) {
		t.Errorf("the terminal was not entered:\n%q", view)
	}
	if !strings.Contains(view, showCursor) || !strings.Contains(view, leaveAltScreen) {
		t.Errorf("the escapes were not restored:\n%q", view)
	}
	if !f.restored {
		t.Error("raw mode was not restored")
	}
}

func TestRunRestoresTheTerminalOnQuit(t *testing.T) {
	f := &fakeTerminal{width: 80, height: 24, keysCh: quitKey()}
	if err := run(context.Background(), New(nil, nil, nil, "", ""), f, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertRestored(t, f)
}

func TestRunRestoresTheTerminalOnPanic(t *testing.T) {
	f := &fakeTerminal{panicOnSize: true, keysCh: make(chan Key)}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = run(context.Background(), New(nil, nil, nil, "", ""), f, nil, nil)
	}()
	if recovered == nil {
		t.Fatal("the size failure did not panic")
	}
	assertRestored(t, f)
}

func TestRunRestoresTheTerminalOnSIGTERM(t *testing.T) {
	f := &fakeTerminal{width: 80, height: 24}
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	if err := run(context.Background(), New(nil, nil, nil, "", ""), f, signals, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertRestored(t, f)
}

func TestRawModeStartsEveryLineAtColumnOne(t *testing.T) {
	f := &fakeTerminal{width: 80, height: 24, keysCh: quitKey()}
	if err := run(context.Background(), New(nil, nil, nil, "", ""), f, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	_, drawn, ok := strings.Cut(f.out.String(), clearScreen)
	if !ok {
		t.Fatal("the screen was never cleared")
	}
	for i := range len(drawn) {
		if drawn[i] != '\n' {
			continue
		}
		if i == 0 || drawn[i-1] != '\r' {
			t.Fatalf("line feed at offset %d is not preceded by a carriage return", i)
		}
	}
}
