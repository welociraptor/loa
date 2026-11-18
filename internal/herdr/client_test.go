package herdr_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loa/internal/herdr"
)

// The fake binary answers with recorded server output, so these tests pin both
// the argument construction and the decoding. It is not a stand-in for a live
// session; that is the manual checklist.
const fake = `#!/bin/sh
printf '%s\n' "$*" >> "$ARGS_FILE"
fixture() { cat "$TESTDATA/$1"; }
case "$1 $2" in
  "status --json") fixture status.json ;;
  "workspace list") fixture workspace_list.json ;;
  "pane list")     fixture pane_list.json ;;
  "agent list")    fixture agent_list.json ;;
  "worktree create") fixture worktree_create.json ;;
  "workspace create") fixture workspace_create.json ;;
  "worktree remove"|"workspace close"|"agent send-keys"|"agent focus"|"workspace focus") echo '{"id":"x","result":{"type":"ok"}}' ;;
  "agent start")   echo '{"id":"x","result":{"type":"ok"}}' ;;
  "agent prompt")
    if [ -n "$BLOCK_PROMPT" ]; then cat "$TESTDATA/error_blocked.json" >&2; exit 1; fi
    echo '{"id":"x","result":{"type":"ok"}}' ;;
  "agent read")    printf 'did the thing\n' ;;
  *) echo "unexpected: $*" >&2; exit 9 ;;
esac
`

type harness struct {
	client   *herdr.Client
	argsFile string
}

func newHarness(t *testing.T, extraEnv ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	// The fixture has to be executable.
	if err := os.WriteFile(bin, []byte(fake), 0o700); err != nil { //nolint:gosec // G306: must be executable
		t.Fatalf("write fake herdr: %v", err)
	}
	argsFile := filepath.Join(dir, "args")

	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("TESTDATA", "testdata")
	for _, kv := range extraEnv {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	return &harness{client: &herdr.Client{Bin: bin}, argsFile: argsFile}
}

// calls returns every invocation the client made, one per line.
func (h *harness) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(h.argsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read args: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
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
}

func TestWorkspaces(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	workspaces, err := h.client.Workspaces(ctx)
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

func TestCreateWorktreeBuildsTheCommandAndDecodesIds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	wt, err := h.client.CreateWorktree(ctx, herdr.WorktreeRequest{
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

	got := h.calls(t)[0]
	if strings.Contains(got, "--cwd") {
		t.Errorf("command %q passes --cwd, which Herdr rejects next to --workspace", got)
	}
	if strings.Contains(got, "--path") {
		t.Errorf("command %q passes --path; the worktree directory is Herdr's configuration", got)
	}
	for _, want := range []string{
		"worktree create",
		"--workspace w1",
		"--branch loa/7.1",
		"--base HEAD",
		"--label loa #7",
		"--no-focus",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command %q is missing %q", got, want)
		}
	}
}

func TestCreateWorktreeNeedsAWorkspace(t *testing.T) {
	h := newHarness(t)
	if _, err := h.client.CreateWorktree(context.Background(), herdr.WorktreeRequest{
		Branch: "b", Base: "HEAD", Label: "l",
	}); err == nil {
		t.Fatal("a worktree without a workspace was accepted")
	}
	if calls := h.calls(t); len(calls) != 0 {
		t.Errorf("the client ran %v", calls)
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
	if got := h.calls(t)[0]; !strings.Contains(got, "--wait") || !strings.Contains(got, "--timeout 90000") {
		t.Errorf("command = %q", got)
	}
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
	if got := h.calls(t)[0]; !strings.Contains(got, "--source recent-unwrapped") || !strings.Contains(got, "--lines 200") {
		t.Errorf("command = %q", got)
	}
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
	got := h.calls(t)[0]
	if !strings.Contains(got, "--kind pi") || !strings.Contains(got, "--pane w3:p1") ||
		!strings.Contains(got, "-- --no-session --tools read,edit") {
		t.Errorf("command = %q", got)
	}
}

func TestUnknownServerErrorSurfacesAsPlainError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\necho 'not json' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // G306: must be executable
		t.Fatalf("write: %v", err)
	}
	_, err := (&herdr.Client{Bin: bin}).Workspaces(context.Background())
	if err == nil || herdr.CodeOf(err) != "" {
		t.Fatalf("err = %v, want a plain error", err)
	}
	if !strings.Contains(err.Error(), "not json") {
		t.Errorf("the CLI's stderr was dropped: %v", err)
	}
}

func TestSendKeysRejectsNothing(t *testing.T) {
	h := newHarness(t)
	if err := h.client.SendKeys(context.Background(), "loa-7-1"); err == nil {
		t.Fatal("an empty key sequence was sent")
	}
	if calls := h.calls(t); len(calls) != 0 {
		t.Errorf("the client ran %v", calls)
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
	calls := h.calls(t)
	if calls[0] != "agent focus loa-7-1" {
		t.Errorf("calls[0] = %q", calls[0])
	}
	if calls[1] != "workspace focus w3" {
		t.Errorf("calls[1] = %q", calls[1])
	}
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
	if got := h.calls(t)[0]; got != "workspace create --cwd /home/u/.local/state/loa/triage/1 --label loa triage" {
		t.Errorf("command = %q", got)
	}
}
