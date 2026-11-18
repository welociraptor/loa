// Package taskspec is the boundary where a proposal becomes queue changes. A
// document is written by a model or a person and applied by a program, so it is
// parsed strictly and validated whole: nothing is created from a file that has
// one bad task in it.
package taskspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Document is what a proposal file contains. A bare array is accepted too, and
// means Tasks only.
type Document struct {
	Tasks        []Task     `json:"tasks"`
	Reprioritise []Priority `json:"reprioritise"`
	// Notes are for the human reading the proposal. They are never applied.
	Notes []string `json:"notes"`
}

// Task is a task to create.
type Task struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Workspace   string   `json:"workspace"`
	Priority    int      `json:"priority"`
	MaxAttempts int      `json:"max_attempts"`
	Scopes      []string `json:"scopes"`
	// Deps are ids of tasks that already exist.
	Deps []int64 `json:"deps"`
	// After names other tasks in this document, by title, that must finish first.
	After []string `json:"after"`
}

// Priority is a change to a task that already exists.
type Priority struct {
	ID       int64 `json:"id"`
	Priority int   `json:"priority"`
}

// Parse reads a document. Unknown fields are rejected rather than ignored: a
// model that invents "assignee" should be told, not silently obeyed.
func Parse(r io.Reader) (Document, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Document{}, fmt.Errorf("taskspec: read: %w", err)
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Document{}, errors.New("taskspec: the document is empty")
	}

	var doc Document
	switch trimmed[0] {
	case '{':
		if err := decode(trimmed, &doc); err != nil {
			return Document{}, fmt.Errorf("taskspec: parse: %w", err)
		}
	case '[':
		if err := decode(trimmed, &doc.Tasks); err != nil {
			return Document{}, fmt.Errorf("taskspec: parse: %w", err)
		}
	default:
		return Document{}, errors.New("taskspec: expected a JSON object or array")
	}
	return doc, nil
}

func decode(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

// Apply is a validated document: positions instead of titles, ready for a store.
type Apply struct {
	Tasks        []Task
	After        [][]int // per task, positions in Tasks
	Reprioritise []Priority
	Notes        []string
}

// Validate checks the whole document and resolves title references. It returns
// the first problem; partial application is never an option.
func Validate(doc Document) (Apply, error) {
	applied := Apply{Tasks: doc.Tasks, Reprioritise: doc.Reprioritise, Notes: doc.Notes}

	positions := make(map[string]int, len(doc.Tasks))
	for i, task := range doc.Tasks {
		if strings.TrimSpace(task.Title) == "" {
			return Apply{}, fmt.Errorf("tasks[%d]: title is required", i)
		}
		if strings.TrimSpace(task.Workspace) == "" {
			return Apply{}, fmt.Errorf("tasks[%d] %q: workspace is required", i, task.Title)
		}
		if task.MaxAttempts < 0 {
			return Apply{}, fmt.Errorf("tasks[%d] %q: max_attempts cannot be negative", i, task.Title)
		}
		for _, scope := range task.Scopes {
			if strings.TrimSpace(scope) == "" {
				return Apply{}, fmt.Errorf("tasks[%d] %q: empty scope", i, task.Title)
			}
		}
		for _, dep := range task.Deps {
			if dep <= 0 {
				return Apply{}, fmt.Errorf("tasks[%d] %q: dep %d is not a task id", i, task.Title, dep)
			}
		}
		if previous, duplicate := positions[task.Title]; duplicate {
			return Apply{}, fmt.Errorf("tasks[%d] and tasks[%d] share the title %q, so `after` cannot refer to one of them",
				previous, i, task.Title)
		}
		positions[task.Title] = i
	}

	applied.After = make([][]int, len(doc.Tasks))
	for i, task := range doc.Tasks {
		for _, title := range task.After {
			if strings.TrimSpace(title) == "" {
				return Apply{}, fmt.Errorf("tasks[%d] %q: empty `after` reference", i, task.Title)
			}
			position, ok := positions[title]
			if !ok {
				return Apply{}, fmt.Errorf("tasks[%d] %q: `after` refers to %q, which is not in this document",
					i, task.Title, title)
			}
			applied.After[i] = append(applied.After[i], position)
		}
	}

	titles := make([]string, len(doc.Tasks))
	for i, task := range doc.Tasks {
		titles[i] = task.Title
	}
	if err := cycles(applied.After, titles); err != nil {
		return Apply{}, err
	}

	seen := make(map[int64]bool, len(doc.Reprioritise))
	for i, change := range doc.Reprioritise {
		if change.ID <= 0 {
			return Apply{}, fmt.Errorf("reprioritise[%d]: %d is not a task id", i, change.ID)
		}
		if seen[change.ID] {
			return Apply{}, fmt.Errorf("reprioritise[%d]: task %d appears twice", i, change.ID)
		}
		seen[change.ID] = true
	}

	for i, note := range doc.Notes {
		if strings.TrimSpace(note) == "" {
			return Apply{}, fmt.Errorf("notes[%d] is empty", i)
		}
	}

	return applied, nil
}

// cycles rejects a document whose dependencies cannot ever be satisfied: a task
// that waits on itself, directly or through others, would sit in the queue
// forever and look like a runner bug.
func cycles(after [][]int, titles []string) error {
	const (
		unvisited = 0
		onPath    = 1
		finished  = 2
	)
	state := make([]int, len(after))

	var visit func(position int, path []string) error
	visit = func(position int, path []string) error {
		switch state[position] {
		case onPath:
			return fmt.Errorf("dependency cycle: %s -> %s",
				strings.Join(path, " -> "), titles[position])
		case finished:
			return nil
		}

		state[position] = onPath
		path = append(path, titles[position])
		for _, next := range after[position] {
			if err := visit(next, path); err != nil {
				return err
			}
		}
		state[position] = finished
		return nil
	}

	for position := range after {
		if state[position] == unvisited {
			if err := visit(position, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
