package herdr_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"loa/internal/herdr"
)

// The fake binary answers with recorded server output, so these tests pin both
// the argument construction and the decoding. It is not a stand-in for a live
// session; that is the manual checklist.
//
// Each invocation is logged as a separate file of NUL-terminated arguments, so
// an argument containing a space or a newline cannot be confused with an
// argument boundary. Failure injection is environment only: the fake never
// reads task text and never writes into the repository.
const fake = `#!/bin/sh
set -e

n=0
if [ -f "$ARGS_COUNTER" ]; then n=$(cat "$ARGS_COUNTER"); fi
n=$((n + 1))
printf '%s' "$n" > "$ARGS_COUNTER"
file="$ARGS_DIR/$(printf '%06d' "$n")"
for a in "$@"; do printf '%s\0' "$a"; done > "$file"

cmd="$1"
if [ "$#" -ge 2 ]; then cmd="$1 $2"; fi

fixture() { cat "$TESTDATA/$1"; }
ok() { printf '%s\n' '{"id":"cli:ok","result":{"type":"ok"}}'; }

if [ -n "${FAIL_COMMAND-}" ] && [ "$cmd" = "$FAIL_COMMAND" ]; then
  fixture "${FAIL_FIXTURE:-error_agent_not_ready.json}" >&2
  exit 1
fi
if [ -n "${STDOUT_FIXTURE_COMMAND-}" ] && [ "$cmd" = "$STDOUT_FIXTURE_COMMAND" ]; then
  fixture "${STDOUT_FIXTURE:-malformed_result.json}"
  exit 0
fi
if [ -n "${SLOW_COMMAND-}" ] && [ "$cmd" = "$SLOW_COMMAND" ]; then
  # A shell builtin loop, not an external sleep: a surviving child would keep
  # the stdout pipe open and defeat the context cancellation being tested.
  while :; do :; done
fi

case "$cmd" in
  "status --json") fixture "${STATUS_FIXTURE:-status.json}" ;;
  "workspace list") fixture "${WORKSPACES_FIXTURE:-workspace_list.json}" ;;
  "agent list") fixture "${AGENTS_FIXTURE:-agent_list.json}" ;;
  "worktree create") fixture "${WORKTREE_FIXTURE:-worktree_create.json}" ;;
  "workspace create") fixture "${WORKSPACE_FIXTURE:-workspace_create.json}" ;;
  "worktree remove"|"workspace close"|"agent send-keys"|"agent focus"|"workspace focus"|"agent wait"|"agent start"|"agent prompt")
    if [ "$cmd" = "agent prompt" ] && [ -n "${BLOCK_PROMPT-}" ]; then
      fixture error_blocked.json >&2
      exit 1
    fi
    ok ;;
  "agent read") printf 'did the thing\n' ;;
  *) printf 'unexpected: %s\n' "$*" >&2; exit 9 ;;
esac
`

type harness struct {
	client  *herdr.Client
	argsDir string
}

func newHarness(t *testing.T, extraEnv ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	// The fixture has to be executable.
	if err := os.WriteFile(bin, []byte(fake), 0o700); err != nil { //nolint:gosec // G306: must be executable
		t.Fatalf("write fake herdr: %v", err)
	}
	argsDir := filepath.Join(dir, "args")
	if err := os.Mkdir(argsDir, 0o700); err != nil {
		t.Fatalf("create args dir: %v", err)
	}

	t.Setenv("ARGS_DIR", argsDir)
	t.Setenv("ARGS_COUNTER", filepath.Join(dir, "args.count"))
	t.Setenv("TESTDATA", testdataDir(t))
	for _, kv := range extraEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("environment %q is not KEY=VALUE", kv)
		}
		t.Setenv(k, v)
	}
	return &harness{client: &herdr.Client{Bin: bin}, argsDir: argsDir}
}

func testdataDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the test source")
	}
	return filepath.Join(filepath.Dir(file), "testdata")
}

// calls returns every invocation the client made, each as its argument list.
func (h *harness) calls(t *testing.T) [][]string {
	t.Helper()
	entries, err := os.ReadDir(h.argsDir)
	if err != nil {
		t.Fatalf("read args dir: %v", err)
	}
	var calls [][]string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(h.argsDir, entry.Name()))
		if err != nil {
			t.Fatalf("read args: %v", err)
		}
		args := strings.Split(string(raw), "\x00")
		if len(args) > 0 && args[len(args)-1] == "" {
			args = args[:len(args)-1]
		}
		calls = append(calls, args)
	}
	return calls
}

func (h *harness) call(t *testing.T, i int) []string {
	t.Helper()
	calls := h.calls(t)
	if i >= len(calls) {
		t.Fatalf("call %d requested, only %d made", i, len(calls))
	}
	return calls[i]
}

func (h *harness) wantCall(t *testing.T, i int, want ...string) {
	t.Helper()
	if got := h.call(t, i); !slices.Equal(got, want) {
		t.Errorf("call %d = %q, want %q", i, got, want)
	}
}

func (h *harness) wantNoCalls(t *testing.T) {
	t.Helper()
	if calls := h.calls(t); len(calls) != 0 {
		t.Errorf("the client ran %q", calls)
	}
}

func clientForScript(t *testing.T, script string) *herdr.Client {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // G306: must be executable
		t.Fatalf("write script: %v", err)
	}
	return &herdr.Client{Bin: bin}
}

func TestCheckStatus(t *testing.T) {
	h := newHarness(t)
	status, err := h.client.CheckStatus(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Server.Version != "0.9.5" || status.Server.Protocol != 22 {
		t.Errorf("status = %+v", status)
	}
	h.wantCall(t, 0, "status", "--json")
}

func TestCheckStatusRejectsStoppedServer(t *testing.T) {
	h := newHarness(t, "STATUS_FIXTURE=status_not_running.json")
	status, err := h.client.CheckStatus(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no server is running") {
		t.Fatalf("err = %v, want a stopped-server error", err)
	}
	if status.Server.Running {
		t.Errorf("status reports a running server: %+v", status)
	}
	if got := herdr.CodeOf(err); got != "" {
		t.Errorf("code = %q, want a plain error", got)
	}
}

func TestCheckStatusRejectsIncompatibleServer(t *testing.T) {
	h := newHarness(t, "STATUS_FIXTURE=status_incompatible.json")
	_, err := h.client.CheckStatus(context.Background())
	if err == nil {
		t.Fatal("an incompatible server was accepted")
	}
	for _, want := range []string{"client 0.9.5", "server 0.8.0", "protocol 21"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestCheckStatusRejectsMalformedStatus(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{"invalid json", "#!/bin/sh\nprintf '{\"client\":'\n", "decode"},
		{"non-json stderr", "#!/bin/sh\nprintf 'not json\\n' >&2\nexit 1\n", "not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := clientForScript(t, tc.script)
			_, err := client.CheckStatus(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
			if got := herdr.CodeOf(err); got != "" {
				t.Errorf("code = %q, want a plain error", got)
			}
		})
	}
}

func TestCallDecodesErrorFromStdout(t *testing.T) {
	h := newHarness(t,
		"STDOUT_FIXTURE_COMMAND=workspace close",
		"STDOUT_FIXTURE=error_workspace_not_found.json",
	)
	err := h.client.CloseWorkspace(context.Background(), "w3")
	if got := herdr.CodeOf(err); got != "workspace_not_found" {
		t.Fatalf("code = %q, want workspace_not_found (%v)", got, err)
	}
	var serverErr *herdr.Error
	if !errors.As(err, &serverErr) {
		t.Fatalf("err = %T, want *herdr.Error", err)
	}
	if !strings.Contains(err.Error(), "workspace close w3") {
		t.Errorf("error does not name the command: %v", err)
	}
}

func TestCallRejectsMalformedResult(t *testing.T) {
	h := newHarness(t,
		"STDOUT_FIXTURE_COMMAND=workspace list",
		"STDOUT_FIXTURE=malformed_result.json",
	)
	_, err := h.client.Workspaces(context.Background())
	if err == nil {
		t.Fatal("a malformed result was accepted")
	}
	for _, want := range []string{"decode result", "workspace list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestWorkspaces(t *testing.T) {
	h := newHarness(t)
	workspaces, err := h.client.Workspaces(context.Background())
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	if len(workspaces) != 2 || workspaces[0].Label != "nixos" || workspaces[1].ID != "w2" {
		t.Fatalf("workspaces = %+v", workspaces)
	}
}

func TestAgentsCarryStatusAndWorkspace(t *testing.T) {
	h := newHarness(t)
	agents, err := h.client.Agents(context.Background())
	if err != nil {
		t.Fatalf("agents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %+v", agents)
	}
	if agents[1].Name != "loa-7-1" || agents[1].Status != "blocked" || agents[1].WorkspaceID != "w3" {
		t.Errorf("named agent = %+v", agents[1])
	}
}

func TestAgentsReportsServerError(t *testing.T) {
	h := newHarness(t, "FAIL_COMMAND=agent list", "FAIL_FIXTURE=error_agent_not_found.json")
	_, err := h.client.Agents(context.Background())
	if got := herdr.CodeOf(err); got != "agent_not_found" {
		t.Fatalf("code = %q, want agent_not_found (%v)", got, err)
	}
}

func TestCreateWorktreeBuildsTheCommandAndDecodesIds(t *testing.T) {
	h := newHarness(t)

	wt, err := h.client.CreateWorktree(context.Background(), herdr.WorktreeRequest{
		WorkspaceID: "w1",
		Branch:      "loa/7.1",
		Base:        "HEAD",
		Label:       "loa #7",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if wt.WorkspaceID != "w3" || wt.PaneID != "w3:p1" || wt.Branch != "loa/7.1" ||
		wt.Repo != "/home/u/Repositories/nixos" {
		t.Errorf("worktree = %+v", wt)
	}

	h.wantCall(t, 0,
		"worktree", "create",
		"--workspace", "w1",
		"--branch", "loa/7.1",
		"--base", "HEAD",
		"--label", "loa #7",
		"--no-focus",
	)
}

func TestCreateWorktreeNeedsAWorkspace(t *testing.T) {
	h := newHarness(t)
	if _, err := h.client.CreateWorktree(context.Background(), herdr.WorktreeRequest{
		Branch: "b", Base: "HEAD", Label: "l",
	}); err == nil {
		t.Fatal("a worktree without a workspace was accepted")
	}
	h.wantNoCalls(t)
}

func TestCreateWorktreeRejectsMissingIds(t *testing.T) {
	h := newHarness(t,
		"STDOUT_FIXTURE_COMMAND=worktree create",
		"STDOUT_FIXTURE=worktree_create_missing_ids.json",
	)
	wt, err := h.client.CreateWorktree(context.Background(), herdr.WorktreeRequest{
		WorkspaceID: "w1",
		Branch:      "loa/7.1",
		Base:        "HEAD",
		Label:       "loa #7",
	})
	if err == nil || !strings.Contains(err.Error(), "no workspace or pane") {
		t.Fatalf("err = %v, want a missing-ids error", err)
	}
	if wt.Path != "/home/u/.local/state/loa/wt/nixos-7.1" {
		t.Errorf("path = %q, want the created checkout still decoded", wt.Path)
	}
	if wt.WorkspaceID != "" || wt.PaneID != "" {
		t.Errorf("worktree = %+v, want empty workspace and pane", wt)
	}
}

func TestCreateWorktreeReportsServerError(t *testing.T) {
	h := newHarness(t, "FAIL_COMMAND=worktree create", "FAIL_FIXTURE=error_workspace_not_found.json")
	_, err := h.client.CreateWorktree(context.Background(), herdr.WorktreeRequest{
		WorkspaceID: "w1",
		Branch:      "loa/7.1",
		Base:        "HEAD",
		Label:       "loa #7",
	})
	if got := herdr.CodeOf(err); got != "workspace_not_found" {
		t.Fatalf("code = %q, want workspace_not_found (%v)", got, err)
	}
}

func TestRemoveWorktreeCommand(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h := newHarness(t)
		if err := h.client.RemoveWorktree(context.Background(), "w3"); err != nil {
			t.Fatalf("remove: %v", err)
		}
		h.wantCall(t, 0, "worktree", "remove", "--workspace", "w3", "--force")
	})

	t.Run("server error", func(t *testing.T) {
		h := newHarness(t, "FAIL_COMMAND=worktree remove", "FAIL_FIXTURE=error_workspace_not_found.json")
		err := h.client.RemoveWorktree(context.Background(), "w3")
		if got := herdr.CodeOf(err); got != "workspace_not_found" {
			t.Fatalf("code = %q, want workspace_not_found (%v)", got, err)
		}
		if !strings.Contains(err.Error(), "worktree remove --workspace w3 --force") {
			t.Errorf("error does not name the command: %v", err)
		}
	})
}

func TestCreateWorkspace(t *testing.T) {
	h := newHarness(t)
	ws, err := h.client.CreateWorkspace(context.Background(), "/home/u/.local/state/loa/triage/1", "loa triage")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if ws.ID != "w4" || ws.PaneID != "w4:p1" {
		t.Fatalf("workspace = %+v", ws)
	}
	h.wantCall(t, 0,
		"workspace", "create",
		"--cwd", "/home/u/.local/state/loa/triage/1",
		"--label", "loa triage",
	)
}

func TestCreateWorkspaceRejectsMissingIds(t *testing.T) {
	h := newHarness(t,
		"STDOUT_FIXTURE_COMMAND=workspace create",
		"STDOUT_FIXTURE=workspace_create_missing_ids.json",
	)
	ws, err := h.client.CreateWorkspace(context.Background(), "/tmp/scratch", "loa triage")
	if err == nil || !strings.Contains(err.Error(), "no workspace or pane") {
		t.Fatalf("err = %v, want a missing-ids error", err)
	}
	if ws != (herdr.CreatedWorkspace{}) {
		t.Errorf("workspace = %+v, want the zero value", ws)
	}
}

func TestCloseWorkspaceCommand(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h := newHarness(t)
		if err := h.client.CloseWorkspace(context.Background(), "w3"); err != nil {
			t.Fatalf("close: %v", err)
		}
		h.wantCall(t, 0, "workspace", "close", "w3")
	})

	t.Run("server error", func(t *testing.T) {
		h := newHarness(t, "FAIL_COMMAND=workspace close", "FAIL_FIXTURE=error_workspace_not_found.json")
		err := h.client.CloseWorkspace(context.Background(), "w3")
		if got := herdr.CodeOf(err); got != "workspace_not_found" {
			t.Fatalf("code = %q, want workspace_not_found (%v)", got, err)
		}
		if !strings.Contains(err.Error(), "workspace close w3") {
			t.Errorf("error does not name the command: %v", err)
		}
	})
}

func TestStartAgentPassesNativeArgsAfterTheSeparator(t *testing.T) {
	h := newHarness(t)
	err := h.client.StartAgent(context.Background(), herdr.StartAgentRequest{
		Name:    "loa-7-1",
		Kind:    "pi",
		PaneID:  "w3:p1",
		Args:    []string{"--no-session", "--tools", "read,edit"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	h.wantCall(t, 0,
		"agent", "start", "loa-7-1",
		"--kind", "pi",
		"--pane", "w3:p1",
		"--timeout", "30000",
		"--", "--no-session", "--tools", "read,edit",
	)
}

func TestStartAgentWithoutArgsOrTimeout(t *testing.T) {
	h := newHarness(t)
	err := h.client.StartAgent(context.Background(), herdr.StartAgentRequest{
		Name:   "loa-7-1",
		Kind:   "pi",
		PaneID: "w3:p1",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	h.wantCall(t, 0, "agent", "start", "loa-7-1", "--kind", "pi", "--pane", "w3:p1")
}

func TestStartAgentReportsNotReady(t *testing.T) {
	h := newHarness(t, "FAIL_COMMAND=agent start", "FAIL_FIXTURE=error_agent_not_ready.json")
	err := h.client.StartAgent(context.Background(), herdr.StartAgentRequest{
		Name:   "loa-7-1",
		Kind:   "pi",
		PaneID: "w3:p1",
	})
	if got := herdr.CodeOf(err); got != "agent_not_ready" {
		t.Fatalf("code = %q, want agent_not_ready (%v)", got, err)
	}
}

func TestPromptReportsTheBlockedCode(t *testing.T) {
	h := newHarness(t, "BLOCK_PROMPT=1")
	err := h.client.Prompt(context.Background(), "loa-7-1", "do it", 10*time.Second)
	if err == nil {
		t.Fatal("a blocked agent was reported as accepted")
	}
	if got := herdr.CodeOf(err); got != "agent_blocked" {
		t.Errorf("code = %q, want agent_blocked (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "agent prompt loa-7-1") {
		t.Errorf("error does not name the command: %v", err)
	}
}

func TestPromptPassesTheTimeout(t *testing.T) {
	h := newHarness(t)
	if err := h.client.Prompt(context.Background(), "loa-7-1", "do it", 90*time.Second); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	h.wantCall(t, 0, "agent", "prompt", "loa-7-1", "do it", "--wait", "--timeout", "90000")
}

func TestPromptWithoutTimeout(t *testing.T) {
	h := newHarness(t)
	if err := h.client.Prompt(context.Background(), "loa-7-1", "do it", 0); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	h.wantCall(t, 0, "agent", "prompt", "loa-7-1", "do it", "--wait")
}

func TestReadReturnsText(t *testing.T) {
	h := newHarness(t)
	out, err := h.client.Read(context.Background(), "loa-7-1", 200)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(out) != "did the thing" {
		t.Errorf("read = %q", out)
	}
	h.wantCall(t, 0,
		"agent", "read", "loa-7-1",
		"--source", "recent-unwrapped",
		"--lines", "200",
		"--format", "text",
	)
}

func TestReadReportsServerError(t *testing.T) {
	h := newHarness(t, "FAIL_COMMAND=agent read", "FAIL_FIXTURE=error_agent_not_found.json")
	_, err := h.client.Read(context.Background(), "loa-7-1", 200)
	if got := herdr.CodeOf(err); got != "agent_not_found" {
		t.Fatalf("code = %q, want agent_not_found (%v)", got, err)
	}
}

func TestSendKeysPassesEachKey(t *testing.T) {
	h := newHarness(t)
	if err := h.client.SendKeys(context.Background(), "loa-7-1", "ctrl+c", "esc"); err != nil {
		t.Fatalf("send keys: %v", err)
	}
	h.wantCall(t, 0, "agent", "send-keys", "loa-7-1", "ctrl+c", "esc")
}

func TestSendKeysRejectsNothing(t *testing.T) {
	h := newHarness(t)
	if err := h.client.SendKeys(context.Background(), "loa-7-1"); err == nil {
		t.Fatal("an empty key sequence was sent")
	}
	h.wantNoCalls(t)
}

func TestSendKeysReportsServerError(t *testing.T) {
	h := newHarness(t, "FAIL_COMMAND=agent send-keys", "FAIL_FIXTURE=error_agent_not_found.json")
	err := h.client.SendKeys(context.Background(), "loa-7-1", "ctrl+c")
	if got := herdr.CodeOf(err); got != "agent_not_found" {
		t.Fatalf("code = %q, want agent_not_found (%v)", got, err)
	}
}

func TestFocusTargetsTheAgentOrTheWorkspace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.client.FocusAgent(ctx, "loa-7-1"); err != nil {
		t.Fatalf("focus agent: %v", err)
	}
	if err := h.client.FocusWorkspace(ctx, "w3"); err != nil {
		t.Fatalf("focus workspace: %v", err)
	}
	h.wantCall(t, 0, "agent", "focus", "loa-7-1")
	h.wantCall(t, 1, "workspace", "focus", "w3")
}

func TestFocusReportsServerError(t *testing.T) {
	cases := []struct {
		name    string
		command string
		fixture string
		code    string
		focus   func(context.Context, *herdr.Client) error
	}{
		{
			"agent", "agent focus", "error_agent_not_found.json", "agent_not_found",
			func(ctx context.Context, c *herdr.Client) error { return c.FocusAgent(ctx, "loa-7-1") },
		},
		{
			"workspace", "workspace focus", "error_workspace_not_found.json", "workspace_not_found",
			func(ctx context.Context, c *herdr.Client) error { return c.FocusWorkspace(ctx, "w3") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "FAIL_COMMAND="+tc.command, "FAIL_FIXTURE="+tc.fixture)
			err := tc.focus(context.Background(), h.client)
			if got := herdr.CodeOf(err); got != tc.code {
				t.Fatalf("code = %q, want %s (%v)", got, tc.code, err)
			}
		})
	}
}

func TestWaitPassesEveryStateAndTimeout(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.client.Wait(ctx, "loa-7-1", 5*time.Second, "blocked", "done"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	h.wantCall(t, 0,
		"agent", "wait", "loa-7-1",
		"--until", "blocked",
		"--until", "done",
		"--timeout", "5000",
	)

	if err := h.client.Wait(ctx, "loa-7-1", 0, "blocked"); err != nil {
		t.Fatalf("wait without timeout: %v", err)
	}
	h.wantCall(t, 1, "agent", "wait", "loa-7-1", "--until", "blocked")
}

func TestWaitReportsServerError(t *testing.T) {
	h := newHarness(t, "FAIL_COMMAND=agent wait", "FAIL_FIXTURE=error_agent_not_found.json")
	err := h.client.Wait(context.Background(), "loa-7-1", time.Second, "blocked")
	if got := herdr.CodeOf(err); got != "agent_not_found" {
		t.Fatalf("code = %q, want agent_not_found (%v)", got, err)
	}
}

func TestInSession(t *testing.T) {
	cases := []struct {
		name  string
		unset bool
		value string
		want  bool
	}{
		{"unset", true, "", false},
		{"empty", false, "", false},
		{"zero", false, "0", false},
		{"one", false, "1", true},
		{"true", false, "true", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_ENV", tc.value)
			if tc.unset {
				if err := os.Unsetenv("HERDR_ENV"); err != nil {
					t.Fatalf("unset HERDR_ENV: %v", err)
				}
			}
			if got := herdr.InSession(); got != tc.want {
				t.Errorf("InSession() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBinDefaultsToHerdr(t *testing.T) {
	dir := t.TempDir()
	// The script's own PATH lookup proves the default Bin was resolved from the
	// environment rather than hard-coded to this checkout's binary.
	script := "#!/bin/sh\nprintf '%s\\n' '{\"client\":{\"version\":\"0.9.5\"},\"server\":{\"running\":true,\"version\":\"from-temp-path\",\"protocol\":22,\"compatible\":true}}'\n"
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // G306: must be executable
		t.Fatalf("write fake herdr: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	status, err := (&herdr.Client{}).CheckStatus(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Server.Version != "from-temp-path" {
		t.Errorf("server version = %q, want the temp PATH binary", status.Server.Version)
	}
}

func TestArgumentsAreNeverShellInterpreted(t *testing.T) {
	h := newHarness(t)
	t.Chdir(t.TempDir())

	ctx := context.Background()
	label := "loa #7 --label\n$(touch pwned) 'quoted' \"double\" ; echo"
	if _, err := h.client.CreateWorkspace(ctx, "/tmp/a b/c\nd", label); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	h.wantCall(t, 0, "workspace", "create", "--cwd", "/tmp/a b/c\nd", "--label", label)

	prompt := "line one\nline two --until done $(touch pwned)"
	if err := h.client.Prompt(ctx, "loa-7-1", prompt, 10*time.Second); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	h.wantCall(t, 1, "agent", "prompt", "loa-7-1", prompt, "--wait", "--timeout", "10000")

	native := []string{"--no-session", "$(touch pwned)", "a b\nc", "--label"}
	if err := h.client.StartAgent(ctx, herdr.StartAgentRequest{
		Name:    "loa-7-1",
		Kind:    "pi",
		PaneID:  "w3:p1",
		Args:    native,
		Timeout: time.Second,
	}); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	h.wantCall(t, 2, append([]string{
		"agent", "start", "loa-7-1",
		"--kind", "pi",
		"--pane", "w3:p1",
		"--timeout", "1000",
		"--",
	}, native...)...)

	if _, err := os.Stat("pwned"); !os.IsNotExist(err) {
		t.Fatalf("argument text was interpreted by a shell: %v", err)
	}
}

func TestExecUsesCommandContext(t *testing.T) {
	h := newHarness(t, "SLOW_COMMAND=agent wait")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := h.client.Wait(ctx, "loa-7-1", 0, "done")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled call was reported as successful")
	}
	if elapsed > time.Second {
		t.Fatalf("call took %v after a 25ms deadline", elapsed)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("context err = %v, want deadline exceeded", ctx.Err())
	}
}

func TestUnknownServerErrorSurfacesAsPlainError(t *testing.T) {
	client := clientForScript(t, "#!/bin/sh\necho 'not json' >&2\nexit 1\n")
	_, err := client.Workspaces(context.Background())
	if err == nil || herdr.CodeOf(err) != "" {
		t.Fatalf("err = %v, want a plain error", err)
	}
	if !strings.Contains(err.Error(), "not json") {
		t.Errorf("the CLI's stderr was dropped: %v", err)
	}
}

func TestCodeOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain", errors.New("plain"), ""},
		{"herdr", &herdr.Error{Code: "agent_blocked", Message: "waiting"}, "agent_blocked"},
		{"wrapped", fmt.Errorf("prompt: %w", &herdr.Error{Code: "agent_not_found", Message: "gone"}), "agent_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := herdr.CodeOf(tc.err); got != tc.want {
				t.Errorf("CodeOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
