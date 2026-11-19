package taskspec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loa/internal/taskspec"
)

func parseVerdict(t *testing.T, doc string) taskspec.Review {
	t.Helper()
	review, err := taskspec.ParseVerdict(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse verdict: %v", err)
	}
	return review
}

func TestParseVerdictAcceptsEveryVerdict(t *testing.T) {
	for _, verdict := range []taskspec.Verdict{taskspec.Approve, taskspec.Changes, taskspec.Blocked} {
		got := parseVerdict(t, `{"verdict":"`+string(verdict)+`","notes":[{"file":"a.go","issue":"wrong","suggestion":"fix it"}]}`)
		if got.Verdict != verdict {
			t.Errorf("verdict = %q, want %q", got.Verdict, verdict)
		}
		if len(got.Notes) != 1 || got.Notes[0].File != "a.go" {
			t.Errorf("notes = %+v", got.Notes)
		}
	}
}

func TestParseVerdictRejectsWhatItCannotActOn(t *testing.T) {
	cases := map[string]string{
		"unknown field":   `{"verdict":"approve","notes":[],"confidence":0.9}`,
		"unknown note":    `{"verdict":"approve","notes":[{"file":"a.go","issue":"x","line":4}]}`,
		"missing verdict": `{"notes":[]}`,
		"unknown verdict": `{"verdict":"lgtm","notes":[]}`,
		"empty":           "  ",
		"not an object":   `["approve"]`,
		"trailing data":   `{"verdict":"approve","notes":[]} {"again":true}`,
		"missing file":    `{"verdict":"changes","notes":[{"issue":"wrong"}]}`,
		"missing issue":   `{"verdict":"changes","notes":[{"file":"a.go"}]}`,
		"bad task":        `{"verdict":"changes","notes":[],"tasks":[{"title":"follow-up"}]}`,
	}
	for name, doc := range cases {
		if _, err := taskspec.ParseVerdict(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseVerdictValidatesProposalTasks(t *testing.T) {
	review := parseVerdict(t, `{"verdict":"changes","notes":[],"tasks":[
		{"title":"fix the parser","workspace":"nixos","body":"add the missing case"}
	]}`)
	if len(review.Tasks) != 1 || review.Tasks[0].Title != "fix the parser" {
		t.Fatalf("tasks = %+v", review.Tasks)
	}
}

func TestParseNotesRoundTripsTheStoredArray(t *testing.T) {
	notes, err := taskspec.ParseNotes(`[{"file":"a.go","issue":"wrong","suggestion":""}]`)
	if err != nil {
		t.Fatalf("parse notes: %v", err)
	}
	if len(notes) != 1 || notes[0].File != "a.go" || notes[0].Suggestion != "" {
		t.Errorf("notes = %+v", notes)
	}
	if none, err := taskspec.ParseNotes(""); err != nil || none != nil {
		t.Errorf("empty notes = %+v, %v", none, err)
	}
}

func TestSaveProposalWritesThePlanFormatWithoutApplyingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proposals")
	path, err := taskspec.SaveProposal(dir, []taskspec.Task{{
		Title: "follow-up", Body: "do it", Workspace: "nixos",
	}}, time.Date(2026, 11, 18, 18, 57, 22, 0, time.UTC))
	if err != nil {
		t.Fatalf("save proposal: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("path = %q, want it under %q", path, dir)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the proposal file this test just wrote
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	doc, err := taskspec.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse proposal: %v", err)
	}
	if len(doc.Tasks) != 1 || doc.Tasks[0].Title != "follow-up" {
		t.Errorf("proposal = %s", raw)
	}
}
