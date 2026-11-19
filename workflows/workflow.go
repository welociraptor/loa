// Package workflows validates the document that describes how a plan is worked.
// A workflow is written by a person and versioned with the repository, so it is
// parsed strictly: a misspelled field is a typo to fix, not something to ignore.
package workflows

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// MergePolicy is what the coordinator may do with an approved, green change.
type MergePolicy string

// The two policies a workflow may declare. "auto" is only meaningful with a
// check command; the merge guard in loa still decides every individual merge.
const (
	MergeAuto   MergePolicy = "auto"
	MergeManual MergePolicy = "manual"
)

// Workflow is one workflow file.
type Workflow struct {
	// Prompts maps a role to the prompt file its worker reads.
	Prompts map[string]string `json:"prompts"`
	// Check is the command whose exit status gates an auto-merge.
	Check string `json:"check"`
	// Merge is what may happen to an approved change.
	Merge MergePolicy `json:"merge"`
	// Stages are the roles, in order, a plan passes through.
	Stages []string `json:"stages"`
}

// Load reads and validates a workflow file.
func Load(path string) (Workflow, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the workflow path is named by the user
	if err != nil {
		return Workflow{}, fmt.Errorf("workflows: %w", err)
	}
	defer func() { _ = file.Close() }()
	return Parse(file)
}

// Parse reads a workflow document. Unknown fields are rejected rather than
// ignored, and validation is whole-document: one bad stage applies nothing.
func Parse(r io.Reader) (Workflow, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflows: read: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Workflow{}, errors.New("workflows: the document is empty")
	}
	if trimmed[0] != '{' {
		return Workflow{}, errors.New("workflows: expected a JSON object")
	}

	var workflow Workflow
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&workflow); err != nil {
		return Workflow{}, fmt.Errorf("workflows: parse: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Workflow{}, errors.New("workflows: unexpected data after the JSON document")
		}
		return Workflow{}, fmt.Errorf("workflows: parse: %w", err)
	}

	if err := Validate(workflow); err != nil {
		return Workflow{}, err
	}
	return workflow, nil
}

// Validate checks a workflow's fields. It names the field that is wrong, since
// that is what a person editing the file has to fix.
func Validate(workflow Workflow) error {
	if len(workflow.Prompts) == 0 {
		return errors.New("workflows: prompts is required")
	}
	for role, path := range workflow.Prompts {
		if strings.TrimSpace(role) == "" {
			return errors.New("workflows: a prompt role is empty")
		}
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("workflows: prompts[%q] is empty", role)
		}
	}

	switch workflow.Merge {
	case MergeAuto:
		if strings.TrimSpace(workflow.Check) == "" {
			return errors.New("workflows: merge is auto, so check is required")
		}
	case MergeManual:
	default:
		return fmt.Errorf("workflows: merge is %q, want auto or manual", workflow.Merge)
	}
	if strings.TrimSpace(workflow.Check) == "" {
		return errors.New("workflows: check is required")
	}

	if len(workflow.Stages) == 0 {
		return errors.New("workflows: stages is required")
	}
	seen := make(map[string]bool, len(workflow.Stages))
	for i, stage := range workflow.Stages {
		if strings.TrimSpace(stage) == "" {
			return fmt.Errorf("workflows: stages[%d] is empty", i)
		}
		if seen[stage] {
			return fmt.Errorf("workflows: stage %q appears twice", stage)
		}
		seen[stage] = true
	}
	return nil
}
