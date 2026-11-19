package taskspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Verdict is a reviewer's conclusion. It is deliberately a closed set: an
// unknown word cannot be treated as an approval by a later reader.
type Verdict string

// The three answers a review may give.
const (
	Approve Verdict = "approve"
	Changes Verdict = "changes"
	Blocked Verdict = "blocked"
)

// Note is one finding in a verdict. File and issue say what is wrong; the
// suggestion is optional because a reviewer may only be able to name the
// problem.
type Note struct {
	File       string `json:"file"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion"`
}

// Review is what a reviewer writes to .loa/verdict.json. Tasks is the same
// plan format an import uses, so a `changes` verdict can become a proposal
// without a second language.
type Review struct {
	Verdict Verdict `json:"verdict"`
	Notes   []Note  `json:"notes"`
	Tasks   []Task  `json:"tasks,omitempty"`
}

// ParseVerdict reads a review document. Like a proposal it is parsed strictly:
// an invented field or an unknown verdict is refused, and a malformed document
// names the reason rather than being guessed at.
func ParseVerdict(r io.Reader) (Review, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Review{}, fmt.Errorf("taskspec: read verdict: %w", err)
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Review{}, errors.New("taskspec: the verdict document is empty")
	}
	if trimmed[0] != '{' {
		return Review{}, errors.New("taskspec: expected a JSON object for a verdict")
	}

	var review Review
	if err := decode(trimmed, &review); err != nil {
		return Review{}, fmt.Errorf("taskspec: parse verdict: %w", err)
	}
	if err := review.Validate(); err != nil {
		return Review{}, fmt.Errorf("taskspec: verdict: %w", err)
	}
	return review, nil
}

// Validate checks the complete review. It returns the first problem, because a
// verdict with one bad note is not a verdict to act on.
func (v Review) Validate() error {
	switch v.Verdict {
	case Approve, Changes, Blocked:
	case "":
		return errors.New("a verdict is required; use approve, changes or blocked")
	default:
		return fmt.Errorf("unknown verdict %q; use approve, changes or blocked", v.Verdict)
	}

	if err := validateNotes(v.Notes); err != nil {
		return err
	}
	if len(v.Tasks) > 0 {
		if _, err := Validate(Document{Tasks: v.Tasks}); err != nil {
			return fmt.Errorf("tasks: %w", err)
		}
	}
	return nil
}

func validateNotes(notes []Note) error {
	for i, note := range notes {
		if strings.TrimSpace(note.File) == "" {
			return fmt.Errorf("notes[%d]: file is required", i)
		}
		if strings.TrimSpace(note.Issue) == "" {
			return fmt.Errorf("notes[%d] %s: issue is required", i, note.File)
		}
	}
	return nil
}

// ParseNotes reads the notes stored on a task. The store keeps the JSON the
// parser accepted, so this is the read half of that record, not a second
// format.
func ParseNotes(raw string) ([]Note, error) {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] != '[' {
		return nil, errors.New("taskspec: stored verdict notes are not a JSON array")
	}

	var notes []Note
	if err := decode(trimmed, &notes); err != nil {
		return nil, fmt.Errorf("taskspec: parse verdict notes: %w", err)
	}
	if err := validateNotes(notes); err != nil {
		return nil, fmt.Errorf("taskspec: verdict notes: %w", err)
	}
	return notes, nil
}

// SaveProposal writes a changes verdict's tasks in the plan format under dir.
// Nothing applies the file: a person does that with `loa import`.
func SaveProposal(dir string, tasks []Task, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("taskspec: proposals dir: %w", err)
	}
	if tasks == nil {
		tasks = []Task{}
	}
	raw, err := json.MarshalIndent(Document{Tasks: tasks}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("taskspec: encode proposal: %w", err)
	}

	path := filepath.Join(dir, now.UTC().Format("20060102T150405.000000000Z")+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("taskspec: write proposal: %w", err)
	}
	return path, nil
}
