package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type daemonProc struct {
	dir    string
	sock   string
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	waited bool
}

// startDaemonIn starts the built binary as a real supervisor would: its own
// process, its own runtime directory, and no Herdr on PATH.
func startDaemonIn(t *testing.T, dir string) *daemonProc {
	t.Helper()
	return startDaemonWithArgs(t, dir, "daemon")
}

func startDaemonWithArgs(t *testing.T, dir string, args ...string) *daemonProc {
	t.Helper()
	p := &daemonProc{
		dir:  dir,
		sock: filepath.Join(dir, "loa", "sock"),
		cmd:  exec.Command(binary, args...),
	}
	p.cmd.Env = daemonEnv(dir)
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		if !p.waited {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		// A stale path is not enough: after SIGKILL the file is still there. Only
		// a connection proves the new daemon owns the socket.
		conn, err := net.DialTimeout("unix", p.sock, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not accept on %s: %v; stderr:\n%s", p.sock, err, p.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p
}

func daemonEnv(dir string) []string {
	env := []string{
		"XDG_RUNTIME_DIR=" + dir,
		"LOA_DB=" + filepath.Join(dir, "tasks.db"),
		// A directory that exists but has no herdr: the daemon must serve anyway.
		"PATH=" + filepath.Join(dir, "empty-bin"),
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") || strings.HasPrefix(kv, "PATH=") ||
			strings.HasPrefix(kv, "HERDR_") || strings.HasPrefix(kv, "LOA_DB=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func (p *daemonProc) wait(t *testing.T) int {
	t.Helper()
	if p.waited {
		t.Fatal("daemon waited twice")
	}
	err := p.cmd.Wait()
	p.waited = true
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	t.Fatalf("wait for daemon: %v", err)
	return 0
}

func runDaemonCLI(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run %v: %v", args, err)
		}
		return out.String(), errOut.String(), exit.ExitCode()
	}
	return out.String(), errOut.String(), 0
}

func TestDaemonStatusAnswersWithoutRunOrHerdr(t *testing.T) {
	p := startDaemonIn(t, t.TempDir())

	out, errOut, code := runDaemonCLI(t, daemonEnv(p.dir), "daemon", "-status")
	if code != 0 {
		t.Fatalf("daemon -status exited %d: %s", code, errOut)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("status is not one line: %q", out)
	}
	for _, want := range []string{"pid " + strconv.Itoa(p.cmd.Process.Pid), "up ", "protocol 1", "no active run"} {
		if !strings.Contains(out, want) {
			t.Errorf("status = %q, missing %q", out, want)
		}
	}

	dirInfo, err := os.Stat(filepath.Join(p.dir, "loa"))
	if err != nil {
		t.Fatalf("runtime dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("runtime directory mode = %o, want 700", got)
	}
	sockInfo, err := os.Stat(p.sock)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	if got := sockInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode = %o, want 600", got)
	}
}

func TestDaemonSecondInstanceRefuses(t *testing.T) {
	p := startDaemonIn(t, t.TempDir())

	_, errOut, code := runDaemonCLI(t, daemonEnv(p.dir), "daemon")
	if code != 1 {
		t.Fatalf("second daemon exited %d, want 1; stderr %q", code, errOut)
	}
	if !strings.Contains(errOut, "already running") || !strings.Contains(errOut, strconv.Itoa(p.cmd.Process.Pid)) {
		t.Errorf("second daemon stderr = %q, want the holder's pid", errOut)
	}
	if _, err := (&daemonClient{path: p.sock}).status(); err != nil {
		t.Errorf("the first daemon stopped answering: %v", err)
	}
}

func TestDaemonReclaimsAStaleSocketAfterSigkill(t *testing.T) {
	dir := t.TempDir()
	first := startDaemonIn(t, dir)
	firstPID := first.cmd.Process.Pid

	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = first.wait(t)

	// SIGKILL leaves the path behind; the lock does not survive it.
	if _, err := os.Stat(first.sock); err != nil {
		t.Fatalf("the killed daemon's socket is gone, so there is nothing to reclaim: %v", err)
	}

	second := startDaemonIn(t, dir)
	out, errOut, code := runDaemonCLI(t, daemonEnv(dir), "daemon", "-status")
	if code != 0 {
		t.Fatalf("status after reclaim exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "pid "+strconv.Itoa(second.cmd.Process.Pid)) {
		t.Errorf("status = %q, want the new daemon's pid %d (old %d)", out, second.cmd.Process.Pid, firstPID)
	}
}

func TestDaemonSigtermRemovesTheSocket(t *testing.T) {
	p := startDaemonIn(t, t.TempDir())

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := p.wait(t); code != 0 {
		t.Fatalf("daemon exited %d after SIGTERM, want 0; stderr:\n%s", code, p.stderr.String())
	}
	if _, err := os.Stat(p.sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still exists after SIGTERM: %v", err)
	}
}

func TestDaemonRefusesAWrongVersionClient(t *testing.T) {
	p := startDaemonIn(t, t.TempDir())

	conn, err := net.Dial("unix", p.sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(`{"v":2,"do":"ping"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	var resp struct {
		V     int    `json:"v"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if resp.OK || resp.V != 1 || resp.Error != "protocol 2 is newer than this daemon (1)" {
		t.Errorf("refusal = %+v", resp)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("the connection stayed open after the version refusal")
	}
}

func TestRunJSONThroughTheDaemonKeepsItsShape(t *testing.T) {
	dir := t.TempDir()
	env := daemonEnv(dir)
	if _, errOut, code := runDaemonCLI(t, env, "add", "-workspace", "loa", "-title", "runs in the daemon"); code != 0 {
		t.Fatalf("add exited %d: %s", code, errOut)
	}
	startDaemonIn(t, dir)

	stdout, stderr, code := runDaemonCLI(t, env, "run", "-json", "-workers", "1", "-max", "1")
	if code != 0 {
		t.Fatalf("run -json exited %d: %s", code, stderr)
	}
	var got struct {
		Run struct {
			ID        int64  `json:"id"`
			StartedAt string `json:"started_at"`
			EndedAt   string `json:"ended_at"`
			Workers   int    `json:"workers"`
			MaxTasks  int    `json:"max_tasks"`
			Done      int    `json:"done"`
			Failed    int    `json:"failed"`
		} `json:"run"`
		Tasks []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
			Branch string `json:"branch"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("run -json is not one JSON document: %v: %q", err, stdout)
	}
	if got.Run.ID == 0 || got.Run.StartedAt == "" || got.Run.EndedAt == "" {
		t.Errorf("run = %+v, want the recorded row", got.Run)
	}
	if got.Run.Workers != 1 || got.Run.MaxTasks != 1 {
		t.Errorf("budget = %d/%d, want the request's 1/1", got.Run.Workers, got.Run.MaxTasks)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].ID != 1 || got.Tasks[0].Status != "failed" {
		t.Errorf("tasks = %+v, want the one failed attempt without a session", got.Tasks)
	}
	if got.Run.Done != 0 || got.Run.Failed != 1 {
		t.Errorf("counts = (%d, %d), want (0, 1)", got.Run.Done, got.Run.Failed)
	}
}

// daemonClient is the small part of the protocol this test needs; the CLI's own
// client is exercised through `daemon -status`.
type daemonClient struct {
	path string
}

func (c *daemonClient) status() (struct{}, error) {
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return struct{}{}, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(`{"v":1,"do":"status"}` + "\n")); err != nil {
		return struct{}{}, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return struct{}{}, err
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return struct{}{}, err
	}
	if !resp.OK {
		return struct{}{}, fmt.Errorf("status refused")
	}
	return struct{}{}, nil
}

func TestDaemonStatusFailsWithTheReasonWhenNoDaemonRuns(t *testing.T) {
	dir := t.TempDir()
	_, errOut, code := runDaemonCLI(t, daemonEnv(dir), "daemon", "-status")
	if code != 1 {
		t.Fatalf("daemon -status exited %d, want 1; stderr %q", code, errOut)
	}
	if !strings.Contains(errOut, "connect") || !strings.Contains(errOut, "sock") {
		t.Errorf("stderr = %q, want the connection reason", errOut)
	}
}

func TestPauseAndResumeCommandsAndHelp(t *testing.T) {
	p := startDaemonIn(t, t.TempDir())
	env := daemonEnv(p.dir)

	out, errOut, code := runDaemonCLI(t, env, "pause")
	if code != 0 {
		t.Fatalf("pause exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "restart resumes") {
		t.Errorf("pause = %q, want the memory warning", out)
	}
	status, _, code := runDaemonCLI(t, env, "daemon", "-status")
	if code != 0 || !strings.Contains(status, "paused true") {
		t.Fatalf("status after pause = %q (exit %d), want paused true", status, code)
	}

	out, errOut, code = runDaemonCLI(t, env, "resume")
	if code != 0 {
		t.Fatalf("resume exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "resumed") {
		t.Errorf("resume = %q", out)
	}
	status, _, code = runDaemonCLI(t, env, "daemon", "-status")
	if code != 0 || !strings.Contains(status, "paused false") {
		t.Fatalf("status after resume = %q (exit %d), want paused false", status, code)
	}

	help, _, code := runDaemonCLI(t, env, "help")
	if code != 0 {
		t.Fatalf("help exited %d", code)
	}
	for _, want := range []string{"loa pause", "loa resume", "-auto", "restart resumes"} {
		if !strings.Contains(help, want) {
			t.Errorf("help is missing %q", want)
		}
	}
}

func TestDaemonAutoStartsARunWithinOneInterval(t *testing.T) {
	dir := t.TempDir()
	env := daemonEnv(dir)
	if _, errOut, code := runDaemonCLI(t, env, "add", "-workspace", "loa", "-title", "auto work"); code != 0 {
		t.Fatalf("add exited %d: %s", code, errOut)
	}
	startDaemonWithArgs(t, dir, "daemon", "-auto", "-interval", "50ms", "-idle", "0", "-daily", "4", "-workers", "1", "-max", "1")

	deadline := time.Now().Add(2 * time.Second)
	for {
		out, _, code := runDaemonCLI(t, env, "runs", "-json")
		if code == 0 {
			var got struct {
				Runs []struct {
					ID int64 `json:"id"`
				} `json:"runs"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("runs -json: %v: %q", err, out)
			}
			if len(got.Runs) > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("auto did not start a run within one interval")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
