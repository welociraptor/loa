package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultRole is what a task gets when it does not name one. It is also the
// column default, so tasks written before roles existed keep working.
const defaultRole = "implement"

// roleSpec is the fixed part of a role: the prompt file it reads and the tools
// its agent may use. Roles are a column, not an execution path: adding one here
// changes what is asked and what can touch the worktree, not how a run is
// judged.
type roleSpec struct {
	role   string
	prompt string
	tools  []string
}

// implementerTools is the tool set the runner has always granted.
var implementerTools = []string{"read", "edit", "write", "bash", "grep", "find", "ls"}

// reviewTools is read-only plus write: a planner or reviewer produces a
// document, not a change to the worktree.
var reviewTools = []string{"read", "write", "grep", "find", "ls"}

// roles maps every accepted name to its prompt. The short names are the
// canonical ones the plan uses; the file names are accepted too, because a
// person adding a task should not have to remember an alias.
var roles = map[string]roleSpec{
	"implement":   {role: "implement", prompt: "implementer.md", tools: implementerTools},
	"implementer": {role: "implementer", prompt: "implementer.md", tools: implementerTools},
	"plan":        {role: "plan", prompt: "planner.md", tools: reviewTools},
	"planner":     {role: "planner", prompt: "planner.md", tools: reviewTools},
	"review":      {role: "review", prompt: "reviewer.md", tools: reviewTools},
	"reviewer":    {role: "reviewer", prompt: "reviewer.md", tools: reviewTools},
}

// specForRole resolves a role. An unknown role fails the task rather than
// falling back to the implementer: a misspelling must not silently grant bash.
func specForRole(role string) (roleSpec, error) {
	if strings.TrimSpace(role) == "" {
		role = defaultRole
	}
	spec, ok := roles[role]
	if !ok {
		return roleSpec{}, fmt.Errorf("unknown role %q; known roles are implement, plan, review", role)
	}
	return spec, nil
}

// readPrompt loads the role's prompt file from dir. The error names the file
// because that is what a person has to add or fix.
func readPrompt(dir string, spec roleSpec) ([]byte, error) {
	path := filepath.Join(dir, spec.prompt)
	content, err := os.ReadFile(path) //nolint:gosec // G304: the prompt directory is configuration, not task text
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("role %q has no prompt file %s", spec.role, path)
		}
		return nil, fmt.Errorf("read prompt %s: %w", path, err)
	}
	return content, nil
}

// promptHash is the content hash recorded at dispatch. Hashing the file bytes,
// not the rendered text, means the same prompt is attributable across tasks
// with different fields.
func promptHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// defaultPromptsDir finds the repository's prompts directory from the working
// directory. The files are repository data, not embedded state; a binary run
// somewhere else can still be pointed at them with -prompts.
func defaultPromptsDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "prompts"
	}
	for {
		candidate := filepath.Join(dir, "prompts")
		if info, err := os.Stat(filepath.Join(candidate, "implementer.md")); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "prompts"
		}
		dir = parent
	}
}
