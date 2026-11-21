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
	for _, want := range []string{"pid " + strconv.Itoa(p.cmd.Process.Pid), "up ", "protocol 1", "paused false", "auto off", "db " + filepath.Join(p.dir, "tasks.db"), "no active run"} {
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

func TestRunRefusesADatabaseFlagAndCreatesNoFile(t *testing.T) {
	p := startDaemonIn(t, t.TempDir())
	other := filepath.Join(p.dir, "other.db")

	_, errOut, code := runDaemonCLI(t, daemonEnv(p.dir), "run", "-db", other)
	if code != 2 {
		t.Fatalf("run -db exited %d, want the flag parse refusal (2); stderr %q", code, errOut)
	}
	if !strings.Contains(errOut, "-db") {
		t.Errorf("stderr = %q, want the refused flag named", errOut)
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run created or migrated %s: %v", other, err)
	}
}

func TestRunRefusesTheOwnerFlag(t *testing.T) {
	// The runner computes host/pid itself; a caller-supplied owner would be
	// accepted and ignored, so the flag is gone rather than misleading.
	_, errOut, code := runDaemonCLI(t, daemonEnv(t.TempDir()), "run", "-owner", "someone")
	if code != 2 || !strings.Contains(errOut, "-owner") {
		t.Errorf("run -owner exited %d with %q, want the flag parse refusal", code, errOut)
	}
}

func TestClientCommandsRefuseTheDatabaseFlag(t *testing.T) {
	p := startDaemonIn(t, t.TempDir())
	other := filepath.Join(p.dir, "other.db")

	for _, command := range []string{"pause", "resume", "stop"} {
		_, errOut, code := runDaemonCLI(t, daemonEnv(p.dir), command, "-db", other)
		if code != 2 || !strings.Contains(errOut, "-db") {
			t.Errorf("%s -db exited %d with %q, want the flag parse refusal", command, code, errOut)
		}
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a client command created or migrated %s: %v", other, err)
	}
}

func TestDaemonStatusRefusesTheDatabaseFlag(t *testing.T) {
	p := startDaemonIn(t, t.TempDir())
	other := filepath.Join(p.dir, "other.db")

	_, errOut, code := runDaemonCLI(t, daemonEnv(p.dir), "daemon", "-status", "-db", other)
	if code != 1 || !strings.Contains(errOut, "the daemon decides the database") {
		t.Errorf("daemon -status -db exited %d with %q, want the client refusal", code, errOut)
	}
}

func TestDaemonRefusesAnInvalidRunBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"workers", []string{"daemon", "-workers", "0"}, []string{"-workers", "positive"}},
		{"max", []string{"daemon", "-max", "0"}, []string{"-max", "positive"}},
		{"ttl equal to timeout", []string{"daemon", "-ttl", "45m", "-timeout", "45m"}, []string{"-ttl", "-timeout"}},
		{"ttl below timeout", []string{"daemon", "-ttl", "30m", "-timeout", "45m"}, []string{"-ttl", "-timeout"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, errOut, code := runDaemonCLI(t, daemonEnv(dir), tc.args...)
			if code != 1 {
				t.Fatalf("daemon exited %d, want the budget refusal (1); stderr %q", code, errOut)
			}
			for _, want := range tc.want {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr = %q, want %q named", errOut, want)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "loa", "sock")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("refused daemon left a socket: %v", err)
			}
		})
	}
}

func TestRunRefusesAnInvalidRunBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"workers zero", []string{"run", "-workers", "0"}, []string{"-workers", "positive"}},
		{"workers negative", []string{"run", "-workers", "-1"}, []string{"-workers", "positive"}},
		{"max zero", []string{"run", "-max", "0"}, []string{"-max", "positive"}},
		{"timeout zero", []string{"run", "-timeout", "0s"}, []string{"-timeout", "positive"}},
		{"ttl zero", []string{"run", "-ttl", "0s"}, []string{"-ttl", "positive"}},
		{"ttl equal to timeout", []string{"run", "-ttl", "2m", "-timeout", "2m"}, []string{"-ttl", "-timeout"}},
		{"ttl below timeout", []string{"run", "-ttl", "1m", "-timeout", "2m"}, []string{"-ttl", "-timeout"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runtimeDir := filepath.Join(dir, "run")
			if err := os.MkdirAll(filepath.Join(runtimeDir, "loa"), 0o700); err != nil {
				t.Fatalf("runtime dir: %v", err)
			}
			listener, err := net.Listen("unix", filepath.Join(runtimeDir, "loa", "sock"))
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer func() { _ = listener.Close() }()

			_, errOut, code := runDaemonCLI(t, daemonEnv(runtimeDir), tc.args...)
			if code != 1 {
				t.Fatalf("%v exited %d, want the refusal (1); stderr %q", tc.args, code, errOut)
			}
			for _, want := range tc.want {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr = %q, want %q named", errOut, want)
				}
			}

			// A refused request must stop before the socket. The client has exited,
			// so a queued connection would still be accepted; an expired deadline
			// makes an empty queue answer at once.
			ul, ok := listener.(*net.UnixListener)
			if !ok {
				t.Fatalf("listener is %T, want a Unix listener", listener)
			}
			_ = ul.SetDeadline(time.Now())
			conn, err := ul.Accept()
			if err == nil {
				_ = conn.Close()
				t.Errorf("%v reached the daemon socket", tc.args)
			}
		})
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
	for _, want := range []string{"loa pause", "loa resume", "loa stop", "-auto", "restart resumes"} {
		if !strings.Contains(help, want) {
			t.Errorf("help is missing %q", want)
		}
	}
}

func TestDaemonServiceCommandLineParses(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(wd, "..", "..", "contrib", "loa.service")) //nolint:gosec // G304: the unit is in this checkout
	if err != nil {
		t.Fatalf("read contrib/loa.service: %v", err)
	}

	var args []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		for i, field := range fields {
			if field == "daemon" {
				args = fields[i+1:]
				break
			}
		}
	}
	if args == nil {
		t.Fatal("contrib/loa.service has no ExecStart with a daemon subcommand")
	}

	o, _, err := parseDaemonFlags(args)
	if err != nil {
		t.Fatalf("contrib/loa.service ExecStart does not parse: %v", err)
	}
	if !o.auto || o.run.workers != 1 || o.run.maxTasks != 4 || o.idle != 5*time.Minute || o.daily != 12 {
		t.Errorf("unit parses as auto %t, workers %d, max %d, idle %s, daily %d; want the checked-in policy",
			o.auto, o.run.workers, o.run.maxTasks, o.idle, o.daily)
	}
	if o.run.ttl <= o.run.timeout {
		t.Errorf("unit parses as ttl %s, timeout %s; want -ttl to exceed -timeout",
			o.run.ttl, o.run.timeout)
	}
}

func TestDaemonHelpListsThePolicyFlags(t *testing.T) {
	// contrib/loa.service documents this command line; a flag that vanishes
	// from help would make the checked-in policy unparsable.
	_, errOut, code := runDaemonCLI(t, daemonEnv(t.TempDir()), "daemon", "-h")
	if code != 0 {
		t.Fatalf("daemon -h exited %d: %s", code, errOut)
	}
	for _, want := range []string{"-auto", "-workers", "-max", "-idle", "-daily", "tasks settled per day, counted from every run"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("daemon -h is missing %q:\n%s", want, errOut)
		}
	}
}

func TestStopCommandSendsTheStopVerb(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "loa")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	sock := filepath.Join(runtime, "sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type request struct {
		V  int    `json:"v"`
		Do string `json:"do"`
	}
	got := make(chan request, 1)
	go func() {
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var req request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		got <- req
		_ = json.NewEncoder(conn).Encode(struct {
			V  int  `json:"v"`
			OK bool `json:"ok"`
		}{V: 1, OK: true})
	}()

	out, errOut, code := runDaemonCLI(t, daemonEnv(dir), "stop")
	if code != 0 {
		t.Fatalf("stop exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "stop requested") {
		t.Errorf("stop = %q, want the daemon's settlement named", out)
	}
	select {
	case req := <-got:
		if req.V != 1 || req.Do != "stop" {
			t.Errorf("request = %+v, want protocol 1 stop", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop command did not connect to the daemon")
	}
}

func TestRunSecondInterruptKillsTheClient(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "loa")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(runtime, "sock"), Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type request struct {
		V  int    `json:"v"`
		Do string `json:"do"`
	}
	runSeen := make(chan struct{}, 1)
	stopSeen := make(chan struct{}, 1)
	fakeErr := make(chan error, 1)
	release := make(chan struct{})
	defer close(release)
	go func() {
		runConn, err := ln.AcceptUnix()
		if err != nil {
			fakeErr <- fmt.Errorf("accept run: %w", err)
			return
		}
		defer func() { _ = runConn.Close() }()
		var run request
		if err := json.NewDecoder(runConn).Decode(&run); err != nil {
			fakeErr <- fmt.Errorf("read run: %w", err)
			return
		}
		if run.V != 1 || run.Do != "run" {
			fakeErr <- fmt.Errorf("first request = %+v, want protocol 1 run", run)
			return
		}
		runSeen <- struct{}{}

		stopConn, err := ln.AcceptUnix()
		if err != nil {
			fakeErr <- fmt.Errorf("accept stop: %w", err)
			return
		}
		defer func() { _ = stopConn.Close() }()
		var stop request
		if err := json.NewDecoder(stopConn).Decode(&stop); err != nil {
			fakeErr <- fmt.Errorf("read stop: %w", err)
			return
		}
		if stop.V != 1 || stop.Do != "stop" {
			fakeErr <- fmt.Errorf("second request = %+v, want protocol 1 stop", stop)
			return
		}
		stopSeen <- struct{}{}
		// Hold both connections open without an answer: the client is wedged in
		// Stop, which is where the second interrupt has to be able to kill it.
		<-release
	}()

	cmd := exec.Command(binary, "run")
	cmd.Env = daemonEnv(dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start loa run: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	select {
	case <-runSeen:
	case err := <-fakeErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("loa run did not ask the daemon for a run")
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("first interrupt: %v", err)
	}
	select {
	case <-stopSeen:
	case err := <-fakeErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("the first interrupt did not ask the daemon to stop")
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("second interrupt: %v", err)
	}
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("loa run exited 0 after the second interrupt, want the signal to kill it")
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("wait for loa run: %v", err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatalf("loa run exited %v, want SIGINT; stderr:\n%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the second interrupt did not kill a wedged client; stderr:\n%s", stderr.String())
	}
}

func TestDaemonAutoStartsARunWithinOneInterval(t *testing.T) {
	dir := t.TempDir()
	env := daemonEnv(dir)
	if _, errOut, code := runDaemonCLI(t, env, "add", "-workspace", "loa", "-title", "auto work"); code != 0 {
		t.Fatalf("add exited %d: %s", code, errOut)
	}

	// Auto is gated on a fresh probe. Answer it so the scheduler ticks; any
	// other Herdr call may fail, because the run row is what this test needs.
	bin := filepath.Join(dir, "empty-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	script := `#!/bin/sh
case "$1 $2" in
  "status --json") printf '%s\n' '{"client":{"version":"test"},"server":{"running":true,"compatible":true,"protocol":22}}' ;;
  *) echo "fake herdr: unsupported: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatalf("write fake herdr: %v", err)
	}

	startDaemonWithArgs(t, dir, "daemon", "-auto", "-interval", "50ms", "-idle", "0", "-daily", "4", "-workers", "1", "-max", "1")

	status, errOut, code := runDaemonCLI(t, env, "daemon", "-status")
	if code != 0 {
		t.Fatalf("daemon -status exited %d: %s", code, errOut)
	}
	for _, want := range []string{"auto on", "interval 50ms", "daily 4"} {
		if !strings.Contains(status, want) {
			t.Errorf("status = %q, missing %q", status, want)
		}
	}

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
