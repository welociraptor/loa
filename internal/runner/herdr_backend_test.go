package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"loa/internal/herdr"
)

// fakeHerdr is a scripted Herdr CLI. It records every argument (unit-separator
// separated, one call per line) and performs a real `git worktree add`, so
// Open's path and base are checked against an actual checkout. Behaviour that
// a test needs to inject is selected through environment variables; an
// unrecognised command exits non-zero so a new client call cannot pass silently.
const fakeHerdr = `#!/bin/sh
for a in "$@"; do
  printf '%s\037' "$a" >> "$ARGS_FILE"
done
printf '\n' >> "$ARGS_FILE"

server_error() {
  printf '{"id":"cli:%s","error":{"code":"%s","message":"%s"}}' "$1" "$2" "$3" >&2
  exit 1
}

case "$1 $2" in
  "workspace list")
    if [ -n "$FAIL_WORKSPACE_LIST" ]; then
      server_error "workspace:list" "$FAIL_WORKSPACE_LIST" "list failed"
    fi
    cat "$WORKSPACES_FILE"
    ;;
  "worktree create")
    shift 2
    ws=; branch=; base=; label=
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --workspace) ws="$2"; shift 2 ;;
        --branch) branch="$2"; shift 2 ;;
        --base) base="$2"; shift 2 ;;
        --label) label="$2"; shift 2 ;;
        --no-focus) shift ;;
        *) echo "unexpected worktree create argument: $1" >&2; exit 9 ;;
      esac
    done
    if [ -n "$FAIL_WORKTREE_CREATE" ]; then
      server_error "worktree:create" "$FAIL_WORKTREE_CREATE" "create failed"
    fi
    path="$WORKTREE_PATH"
    if [ -z "$path" ]; then
      path="$WORKTREE_ROOT/$ws"
      if ! git -C "$REPO" worktree add -q -b "$branch" "$path" "$base"; then
        echo "fake herdr: git worktree add failed" >&2
        exit 1
      fi
    fi
    printf '{"id":"cli:worktree:create","result":{"root_pane":{"pane_id":"%s:p1"},"workspace":{"workspace_id":"%s","worktree":{"repo_root":"%s"}},"worktree":{"path":"%s","branch":"%s"}}}\n' "$ws" "$ws" "$REPO" "$path" "$branch"
    ;;
  "worktree remove")
    if [ -n "$FAIL_REMOVE" ]; then
      server_error "worktree:remove" "$FAIL_REMOVE" "remove failed"
    fi
    echo '{"id":"cli:worktree:remove","result":{"type":"ok"}}'
    ;;
  "workspace close")
    if [ -n "$FAIL_CLOSE" ]; then
      server_error "workspace:close" "$FAIL_CLOSE" "close failed"
    fi
    echo '{"id":"cli:workspace:close","result":{"type":"ok"}}'
    ;;
  "agent start")
    n=0
    if [ -f "$AGENT_START_COUNT" ]; then
      n=$(cat "$AGENT_START_COUNT")
    fi
    n=$((n + 1))
    printf '%s' "$n" > "$AGENT_START_COUNT"
    if [ -n "$FAIL_AGENT_START" ]; then
      if [ -z "$FAIL_AGENT_START_TIMES" ] || [ "$n" -le "$FAIL_AGENT_START_TIMES" ]; then
        server_error "agent:start" "$FAIL_AGENT_START" "agent is not ready"
      fi
    fi
    echo '{"id":"cli:agent:start","result":{"type":"ok"}}'
    ;;
  "agent prompt")
    if [ -n "$BLOCK_PROMPT" ]; then
      cat "$BLOCKED_FIXTURE" >&2
      exit 1
    fi
    if [ -n "$FAIL_PROMPT" ]; then
      server_error "agent:prompt" "$FAIL_PROMPT" "prompt failed"
    fi
    echo '{"id":"cli:agent:prompt","result":{"type":"ok"}}'
    ;;
  "agent read") printf 'did the thing\n' ;;
  "agent send-keys"|"agent focus"|"workspace focus")
    echo '{"id":"cli:ok","result":{"type":"ok"}}'
    ;;
  *) echo "unexpected: $*" >&2; exit 9 ;;
esac
`

type workspaceSpec struct {
	id    string
	label string
}

// backendFixture owns the fake CLI, its log and a real repository whose
// worktrees Open creates.
type backendFixture struct {
	bin        string
	argsFile   string
	repo       string
	worktrees  string
	workspaces string
}

func newBackendFixture(t *testing.T) *backendFixture {
	t.Helper()
	dir := t.TempDir()
	f := &backendFixture{
		bin:        filepath.Join(dir, "herdr"),
		argsFile:   filepath.Join(dir, "args"),
		repo:       newHerdrRepo(t),
		worktrees:  filepath.Join(dir, "worktrees"),
		workspaces: filepath.Join(dir, "workspaces.json"),
	}

	if err := os.MkdirAll(f.worktrees, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	if err := os.WriteFile(f.bin, []byte(fakeHerdr), 0o700); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatalf("write fake herdr: %v", err)
	}

	blocked := filepath.Join(dir, "error_blocked.json")
	if err := os.WriteFile(blocked, []byte(
		`{"id":"cli:agent:prompt","error":{"code":"agent_blocked","message":"agent is waiting for input"}}`,
	), 0o600); err != nil {
		t.Fatalf("write blocked fixture: %v", err)
	}

	t.Setenv("ARGS_FILE", f.argsFile)
	t.Setenv("REPO", f.repo)
	t.Setenv("WORKTREE_ROOT", f.worktrees)
	t.Setenv("WORKSPACES_FILE", f.workspaces)
	t.Setenv("BLOCKED_FIXTURE", blocked)
	t.Setenv("AGENT_START_COUNT", filepath.Join(dir, "agent-starts"))
	for _, key := range []string{
		"WORKTREE_PATH",
		"FAIL_WORKSPACE_LIST",
		"FAIL_WORKTREE_CREATE",
		"FAIL_REMOVE",
		"FAIL_CLOSE",
		"FAIL_AGENT_START",
		"FAIL_AGENT_START_TIMES",
		"BLOCK_PROMPT",
		"FAIL_PROMPT",
	} {
		t.Setenv(key, "")
	}
	return f
}

// newHerdrRepo creates a repository called repo with one commit on main.
func newHerdrRepo(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	runHerdrGit(t, parent, "init", "-q", "-b", "main", repo)
	runHerdrGit(t, repo, "config", "user.name", "loa test")
	runHerdrGit(t, repo, "config", "user.email", "test@example.invalid")
	runHerdrGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runHerdrGit(t, repo, "add", "-A")
	runHerdrGit(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func runHerdrGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func (f *backendFixture) backend() *Herdr {
	return &Herdr{Client: &herdr.Client{Bin: f.bin}}
}

func (f *backendFixture) setWorkspaces(t *testing.T, specs ...workspaceSpec) {
	t.Helper()
	list := make([]map[string]string, 0, len(specs))
	for _, s := range specs {
		list = append(list, map[string]string{"workspace_id": s.id, "label": s.label})
	}
	raw, err := json.Marshal(map[string]any{
		"id": "cli:workspace:list",
		"result": map[string]any{
			"type":       "workspace_list",
			"workspaces": list,
		},
	})
	if err != nil {
		t.Fatalf("marshal workspaces: %v", err)
	}
	if err := os.WriteFile(f.workspaces, raw, 0o600); err != nil {
		t.Fatalf("write workspaces: %v", err)
	}
}

// calls returns every fake-CLI invocation as its exact argv.
func (f *backendFixture) calls(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile(f.argsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read args: %v", err)
	}
	text := strings.TrimSuffix(string(raw), "\n")
	if text == "" {
		return nil
	}
	var calls [][]string
	for _, line := range strings.Split(text, "\n") {
		parts := strings.Split(line, "\x1f")
		if n := len(parts); n > 0 && parts[n-1] == "" {
			parts = parts[:n-1]
		}
		calls = append(calls, parts)
	}
	return calls
}

func assertArgv(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestResolveExactMatchOnly(t *testing.T) {
	tests := []struct {
		name       string
		workspaces []workspaceSpec
		label      string
		wantID     string
		wantErr    string
	}{
		{
			name:       "one match",
			workspaces: []workspaceSpec{{id: "w1", label: "loa"}},
			label:      "loa",
			wantID:     "w1",
		},
		{
			name:       "no match",
			workspaces: []workspaceSpec{{id: "w1", label: "loa"}},
			label:      "nixos",
			wantErr:    `no Herdr workspace is labelled "nixos"`,
		},
		{
			name: "ambiguous",
			workspaces: []workspaceSpec{
				{id: "w1", label: "loa"},
				{id: "w2", label: "loa"},
			},
			label:   "loa",
			wantErr: `2 Herdr workspaces are labelled "loa"`,
		},
		{
			name:       "case differs",
			workspaces: []workspaceSpec{{id: "w1", label: "loa"}},
			label:      "LOA",
			wantErr:    `no Herdr workspace is labelled "LOA"`,
		},
		{
			name:       "whitespace differs",
			workspaces: []workspaceSpec{{id: "w1", label: "loa"}},
			label:      "loa ",
			wantErr:    `no Herdr workspace is labelled "loa "`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBackendFixture(t)
			f.setWorkspaces(t, tc.workspaces...)

			got, err := f.backend().resolve(context.Background(), tc.label)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("id = %q despite the error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != tc.wantID {
				t.Errorf("id = %q, want %q", got, tc.wantID)
			}
		})
	}

	t.Run("workspace list fails", func(t *testing.T) {
		f := newBackendFixture(t)
		t.Setenv("FAIL_WORKSPACE_LIST", "server_unavailable")

		_, err := f.backend().resolve(context.Background(), "loa")
		if err == nil || herdr.CodeOf(err) != "server_unavailable" {
			t.Fatalf("err = %v, want the server error", err)
		}
	})
}

func TestOpenResolvesBeforeCreatingAnything(t *testing.T) {
	f := newBackendFixture(t)
	f.setWorkspaces(t, workspaceSpec{id: "w1", label: "nixos"})

	_, err := f.backend().Open(context.Background(), OpenRequest{
		TaskID:         7,
		Attempt:        1,
		WorkspaceLabel: "loa",
		Branch:         "loa/7.1",
		Base:           "HEAD",
		AgentName:      "loa-7-1",
	})
	if err == nil || !strings.Contains(err.Error(), `no Herdr workspace is labelled "loa"`) {
		t.Fatalf("err = %v, want the missing label", err)
	}
	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("calls = %q, want only the workspace list", calls)
	}
	assertArgv(t, calls[0], "workspace", "list")
}

func TestOpenCreatesWorktreeAndStartsAgent(t *testing.T) {
	ctx := context.Background()
	f := newBackendFixture(t)
	f.setWorkspaces(t, workspaceSpec{id: "w1", label: "loa"})

	h := f.backend()
	h.AgentArgs = []string{"--no-session", "--tools", "read,edit"}
	handle, err := h.Open(ctx, OpenRequest{
		TaskID:         7,
		Attempt:        1,
		WorkspaceLabel: "loa",
		Branch:         "loa/7.1",
		Base:           "HEAD",
		AgentName:      "loa-7-1",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	base, err := gitOutput(ctx, f.repo, "rev-parse", "HEAD^{commit}")
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	wantPath := filepath.Join(f.worktrees, "w1")
	want := Handle{
		TaskID:      7,
		AgentName:   "loa-7-1",
		WorkspaceID: "w1",
		PaneID:      "w1:p1",
		Worktree:    wantPath,
		Repo:        f.repo,
		Base:        base,
		BaseRef:     "HEAD",
		Branch:      "loa/7.1",
	}
	if !reflect.DeepEqual(handle, want) {
		t.Errorf("handle = %+v, want %+v", handle, want)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "README.md")); err != nil {
		t.Errorf("the fake did not create a real worktree: %v", err)
	}

	calls := f.calls(t)
	if len(calls) != 3 {
		t.Fatalf("calls = %q, want list, create and start", calls)
	}
	assertArgv(t, calls[0], "workspace", "list")
	assertArgv(t, calls[1],
		"worktree", "create",
		"--workspace", "w1",
		"--branch", "loa/7.1",
		"--base", "HEAD",
		"--label", "loa #7",
		"--no-focus",
	)
	assertArgv(t, calls[2],
		"agent", "start", "loa-7-1",
		"--kind", "pi",
		"--pane", "w1:p1",
		"--timeout", "60000",
		"--", "--no-session", "--tools", "read,edit",
	)
}

func TestOpenFallsBackWhenTheBaseRefIsGone(t *testing.T) {
	ctx := context.Background()
	f := newBackendFixture(t)
	f.setWorkspaces(t, workspaceSpec{id: "w1", label: "loa"})

	handle, err := f.backend().Open(ctx, OpenRequest{
		TaskID:         7,
		Attempt:        1,
		WorkspaceLabel: "loa",
		Branch:         "loa/7.1",
		Base:           "loa/6.1",
		FallbackBase:   "HEAD",
		AgentName:      "loa-7-1",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	base, err := gitOutput(ctx, f.repo, "rev-parse", "HEAD^{commit}")
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if handle.Base != base || handle.BaseRef != "HEAD" {
		t.Errorf("handle base = %q/%q, want the configured HEAD commit", handle.BaseRef, handle.Base)
	}
	if _, err := os.Stat(filepath.Join(f.worktrees, "w1", "README.md")); err != nil {
		t.Errorf("the fallback worktree was not created: %v", err)
	}

	calls := f.calls(t)
	if len(calls) != 4 {
		t.Fatalf("calls = %q, want list, a failed create, the fallback create and start", calls)
	}
	assertArgv(t, calls[1],
		"worktree", "create",
		"--workspace", "w1",
		"--branch", "loa/7.1",
		"--base", "loa/6.1",
		"--label", "loa #7",
		"--no-focus",
	)
	assertArgv(t, calls[2],
		"worktree", "create",
		"--workspace", "w1",
		"--branch", "loa/7.1",
		"--base", "HEAD",
		"--label", "loa #7",
		"--no-focus",
	)
}

func TestOpenReportsBothBasesWhenTheFallbackAlsoFails(t *testing.T) {
	f := newBackendFixture(t)
	f.setWorkspaces(t, workspaceSpec{id: "w1", label: "loa"})
	t.Setenv("FAIL_WORKTREE_CREATE", "worktree_exists")

	_, err := f.backend().Open(context.Background(), OpenRequest{
		TaskID:         7,
		Attempt:        1,
		WorkspaceLabel: "loa",
		Branch:         "loa/7.1",
		Base:           "loa/6.1",
		FallbackBase:   "HEAD",
		AgentName:      "loa-7-1",
	})
	if err == nil || !strings.Contains(err.Error(), "create worktree") ||
		!strings.Contains(err.Error(), "from HEAD") {
		t.Fatalf("err = %v, want both base failures named", err)
	}

	calls := f.calls(t)
	if len(calls) != 3 {
		t.Fatalf("calls = %q, want list and both create attempts", calls)
	}
}

func TestOpenRemovesWorktreeWhenBaseIsUnreadable(t *testing.T) {
	f := newBackendFixture(t)
	f.setWorkspaces(t, workspaceSpec{id: "w1", label: "loa"})

	notARepo := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.MkdirAll(notARepo, 0o700); err != nil {
		t.Fatalf("create plain directory: %v", err)
	}
	t.Setenv("WORKTREE_PATH", notARepo)

	_, err := f.backend().Open(context.Background(), OpenRequest{
		TaskID:         7,
		Attempt:        1,
		WorkspaceLabel: "loa",
		Branch:         "loa/7.1",
		Base:           "HEAD",
		AgentName:      "loa-7-1",
	})
	if err == nil || !strings.Contains(err.Error(), "resolve base") {
		t.Fatalf("err = %v, want a failed base read", err)
	}

	calls := f.calls(t)
	if len(calls) != 3 {
		t.Fatalf("calls = %q, want list, create and the cleanup remove", calls)
	}
	assertArgv(t, calls[2], "worktree", "remove", "--workspace", "w1", "--force")
}

func TestOpenFailsWhenWorktreeCreateFails(t *testing.T) {
	f := newBackendFixture(t)
	f.setWorkspaces(t, workspaceSpec{id: "w1", label: "loa"})
	t.Setenv("FAIL_WORKTREE_CREATE", "worktree_exists")

	_, err := f.backend().Open(context.Background(), OpenRequest{
		TaskID:         7,
		Attempt:        1,
		WorkspaceLabel: "loa",
		Branch:         "loa/7.1",
		Base:           "HEAD",
		AgentName:      "loa-7-1",
	})
	if err == nil || !strings.Contains(err.Error(), "create worktree") || herdr.CodeOf(err) != "worktree_exists" {
		t.Fatalf("err = %v, want the wrapped create failure", err)
	}

	calls := f.calls(t)
	if len(calls) != 2 {
		t.Fatalf("calls = %q, want only list and create", calls)
	}
	assertArgv(t, calls[1], "worktree", "create",
		"--workspace", "w1", "--branch", "loa/7.1", "--base", "HEAD",
		"--label", "loa #7", "--no-focus")
}

func TestOpenRemovesWorktreeWhenAgentFails(t *testing.T) {
	f := newBackendFixture(t)
	f.setWorkspaces(t, workspaceSpec{id: "w1", label: "loa"})
	t.Setenv("FAIL_AGENT_START", "agent_failed")
	t.Setenv("FAIL_AGENT_START_TIMES", "1")

	handle, err := f.backend().Open(context.Background(), OpenRequest{
		TaskID:         7,
		Attempt:        1,
		WorkspaceLabel: "loa",
		Branch:         "loa/7.1",
		Base:           "HEAD",
		AgentName:      "loa-7-1",
	})
	if err == nil || !strings.Contains(err.Error(), "start agent") || herdr.CodeOf(err) != "agent_failed" {
		t.Fatalf("err = %v, want the wrapped start failure", err)
	}
	if handle != (Handle{}) {
		t.Errorf("handle = %+v despite the failure", handle)
	}

	calls := f.calls(t)
	if len(calls) != 4 {
		t.Fatalf("calls = %q, want list, create, start and cleanup remove", calls)
	}
	assertArgv(t, calls[3], "worktree", "remove", "--workspace", "w1", "--force")
}

func TestStartAgentRetriesAgentNotReady(t *testing.T) {
	f := newBackendFixture(t)
	t.Setenv("FAIL_AGENT_START", "agent_not_ready")
	t.Setenv("FAIL_AGENT_START_TIMES", "2")

	h := f.backend()
	h.ReadyTimeout = 5 * time.Second
	err := h.startAgent(context.Background(), herdr.StartAgentRequest{
		Name:   "loa-7-1",
		Kind:   "pi",
		PaneID: "w1:p1",
	})
	if err != nil {
		t.Fatalf("start agent: %v", err)
	}

	calls := f.calls(t)
	if len(calls) != 3 {
		t.Fatalf("calls = %q, want two retries and a success", calls)
	}
	for i, call := range calls {
		assertArgv(t, call, "agent", "start", "loa-7-1", "--kind", "pi", "--pane", "w1:p1")
		if !reflect.DeepEqual(call, calls[0]) {
			t.Errorf("call %d = %q, differs from the first", i, call)
		}
	}
}

func TestStartAgentStopsAtDeadline(t *testing.T) {
	f := newBackendFixture(t)
	t.Setenv("FAIL_AGENT_START", "agent_not_ready")
	t.Setenv("FAIL_AGENT_START_TIMES", "100")

	h := f.backend()
	h.ReadyTimeout = 20 * time.Millisecond
	start := time.Now()
	err := h.startAgent(context.Background(), herdr.StartAgentRequest{
		Name:   "loa-7-1",
		Kind:   "pi",
		PaneID: "w1:p1",
	})
	elapsed := time.Since(start)

	if err == nil || herdr.CodeOf(err) != "agent_not_ready" {
		t.Fatalf("err = %v, want the last agent_not_ready", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("startAgent took %s; it did not stop at the deadline", elapsed)
	}
	if calls := f.calls(t); len(calls) < 1 || len(calls) > 2 {
		t.Errorf("calls = %q, want at most one retry after the deadline", calls)
	}
}

func TestPromptMapsAgentBlocked(t *testing.T) {
	t.Run("blocked", func(t *testing.T) {
		f := newBackendFixture(t)
		t.Setenv("BLOCK_PROMPT", "1")

		err := f.backend().Prompt(context.Background(), Handle{AgentName: "loa-7-1"}, "do it", time.Second)
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("err = %v, want ErrBlocked", err)
		}
		if herdr.CodeOf(err) != "agent_blocked" {
			t.Errorf("code = %q, want agent_blocked", herdr.CodeOf(err))
		}
	})

	t.Run("other errors are not blocked", func(t *testing.T) {
		f := newBackendFixture(t)
		t.Setenv("FAIL_PROMPT", "agent_not_found")

		err := f.backend().Prompt(context.Background(), Handle{AgentName: "loa-7-1"}, "do it", time.Second)
		if err == nil {
			t.Fatal("the failed prompt was reported as accepted")
		}
		if errors.Is(err, ErrBlocked) {
			t.Errorf("err = %v was mapped to ErrBlocked", err)
		}
		if herdr.CodeOf(err) != "agent_not_found" {
			t.Errorf("code = %q, want agent_not_found", herdr.CodeOf(err))
		}
	})
}

func TestCloseFallsBackToWorkspaceClose(t *testing.T) {
	t.Run("removes the worktree", func(t *testing.T) {
		f := newBackendFixture(t)
		if err := f.backend().Close(context.Background(), Handle{TaskID: 7, WorkspaceID: "w1"}); err != nil {
			t.Fatalf("close: %v", err)
		}
		calls := f.calls(t)
		if len(calls) != 1 {
			t.Fatalf("calls = %q, want only the remove", calls)
		}
		assertArgv(t, calls[0], "worktree", "remove", "--workspace", "w1", "--force")
	})

	t.Run("falls back to workspace close", func(t *testing.T) {
		f := newBackendFixture(t)
		t.Setenv("FAIL_REMOVE", "worktree_not_found")

		if err := f.backend().Close(context.Background(), Handle{TaskID: 7, WorkspaceID: "w1"}); err != nil {
			t.Fatalf("close: %v, want the workspace close to succeed", err)
		}
		calls := f.calls(t)
		if len(calls) != 2 {
			t.Fatalf("calls = %q, want remove then workspace close", calls)
		}
		assertArgv(t, calls[0], "worktree", "remove", "--workspace", "w1", "--force")
		assertArgv(t, calls[1], "workspace", "close", "w1")
	})

	t.Run("reports both failures", func(t *testing.T) {
		f := newBackendFixture(t)
		t.Setenv("FAIL_REMOVE", "worktree_not_found")
		t.Setenv("FAIL_CLOSE", "workspace_not_found")

		err := f.backend().Close(context.Background(), Handle{TaskID: 7, WorkspaceID: "w1"})
		if err == nil {
			t.Fatal("two failures were reported as success")
		}
		for _, want := range []string{"remove worktree", "close workspace", "worktree_not_found", "workspace_not_found"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, missing %q", err, want)
			}
		}
	})

	t.Run("empty workspace makes no calls", func(t *testing.T) {
		f := newBackendFixture(t)
		if err := f.backend().Close(context.Background(), Handle{TaskID: 7}); err != nil {
			t.Fatalf("close: %v", err)
		}
		if calls := f.calls(t); len(calls) != 0 {
			t.Errorf("calls = %q, want none", calls)
		}
	})
}

func TestTaskWorkspacesKeepsTaskLabels(t *testing.T) {
	f := newBackendFixture(t)
	f.setWorkspaces(t,
		workspaceSpec{id: "w7", label: "loa #7"},
		workspaceSpec{id: "w8", label: "taskflow #8"},
		workspaceSpec{id: "w9", label: "nixos"},
		workspaceSpec{id: "w10", label: "loa #0"},
		workspaceSpec{id: "w11", label: "loa #x"},
		workspaceSpec{id: "w12", label: "loa #9"},
	)

	got, err := f.backend().TaskWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("task workspaces: %v", err)
	}
	want := []TaskWorkspace{
		{TaskID: 7, WorkspaceID: "w7"},
		{TaskID: 8, WorkspaceID: "w8"},
		{TaskID: 9, WorkspaceID: "w12"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("task workspaces = %+v, want %+v", got, want)
	}
}

func TestTaskWorkspacesReportsListFailure(t *testing.T) {
	f := newBackendFixture(t)
	t.Setenv("FAIL_WORKSPACE_LIST", "server_unavailable")

	if _, err := f.backend().TaskWorkspaces(context.Background()); err == nil || herdr.CodeOf(err) != "server_unavailable" {
		t.Fatalf("err = %v, want the server error", err)
	}
}

func TestAgentKindAndReadyTimeoutDefaults(t *testing.T) {
	h := &Herdr{}
	if got := h.agentKind(); got != "pi" {
		t.Errorf("agent kind = %q, want pi", got)
	}
	if got := h.readyTimeout(); got != time.Minute {
		t.Errorf("ready timeout = %s, want 1m", got)
	}

	h.AgentKind = "claude"
	h.ReadyTimeout = 2 * time.Second
	if got := h.agentKind(); got != "claude" {
		t.Errorf("agent kind = %q, want claude", got)
	}
	if got := h.readyTimeout(); got != 2*time.Second {
		t.Errorf("ready timeout = %s, want 2s", got)
	}
}

func TestAgentArgsAppliesTheRoleTools(t *testing.T) {
	h := &Herdr{AgentArgs: []string{"--no-session", "--model", "test"}}
	got := h.agentArgs([]string{"read", "write"})
	want := []string{"--no-session", "--model", "test", "--tools", "read,write"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}

	h.AgentArgs = []string{"--no-session", "--tools", "bash"}
	got = h.agentArgs([]string{"read"})
	want = []string{"--no-session", "--tools", "read"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q, want the configured tools replaced", got)
	}

	if got := h.agentArgs(nil); !reflect.DeepEqual(got, h.AgentArgs) {
		t.Errorf("nil tools changed the configured args: %q", got)
	}
}
