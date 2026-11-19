package taskspec_test

import (
	"strings"
	"testing"

	"loa/internal/taskspec"
)

func parse(t *testing.T, doc string) taskspec.Apply {
	t.Helper()
	parsed, err := taskspec.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	applied, err := taskspec.Validate(parsed)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return applied
}

func TestParseAcceptsAnObjectOrABareArray(t *testing.T) {
	object := parse(t, `{"tasks":[{"title":"a","workspace":"nixos"}],"notes":["look at #3"]}`)
	if len(object.Tasks) != 1 || len(object.Notes) != 1 {
		t.Fatalf("object = %+v", object)
	}

	array := parse(t, `[{"title":"a","workspace":"nixos"}]`)
	if len(array.Tasks) != 1 || array.Tasks[0].Title != "a" {
		t.Fatalf("array = %+v", array)
	}
}

func TestParseRejectsUnknownFieldsAndJunk(t *testing.T) {
	cases := map[string]string{
		"unknown field": `{"tasks":[{"title":"a","workspace":"nixos","assignee":"me"}]}`,
		"empty":         "   ",
		"not json":      "tasks: a, b",
	}
	for name, doc := range cases {
		if _, err := taskspec.Parse(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateResolvesAfterReferences(t *testing.T) {
	applied := parse(t, `{"tasks":[
		{"title":"schema","workspace":"nixos"},
		{"title":"writer","workspace":"nixos","after":["schema"]}
	]}`)

	if len(applied.After[1]) != 1 || applied.After[1][0] != 0 {
		t.Fatalf("after = %v", applied.After)
	}
}

func TestValidateRejectsUnknownAfterAndDuplicateTitles(t *testing.T) {
	cases := map[string]string{
		"unknown after": `{"tasks":[{"title":"a","workspace":"nixos","after":["nope"]}]}`,
		"duplicate":     `{"tasks":[{"title":"a","workspace":"nixos"},{"title":"a","workspace":"nixos"}]}`,
	}
	for name, doc := range cases {
		parsed, err := taskspec.Parse(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if _, err := taskspec.Validate(parsed); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateRejectsCycles(t *testing.T) {
	doc, err := taskspec.Parse(strings.NewReader(`{"tasks":[
		{"title":"a","workspace":"nixos","after":["b"]},
		{"title":"b","workspace":"nixos","after":["a"]}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = taskspec.Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "dependency cycle: a -> b -> a") {
		t.Fatalf("err = %v, want the cycle named", err)
	}
}

func TestValidateRequiresWorkspaceAndSaneNumbers(t *testing.T) {
	cases := map[string]string{
		"no workspace": `{"tasks":[{"title":"a"}]}`,
		"no title":     `{"tasks":[{"workspace":"nixos"}]}`,
		"bad dep":      `{"tasks":[{"title":"a","workspace":"nixos","deps":[-1]}]}`,
		"bad attempts": `{"tasks":[{"title":"a","workspace":"nixos","max_attempts":-1}]}`,
		"bad scope":    `{"tasks":[{"title":"a","workspace":"nixos","scopes":[" "}}]}`,
		"bad priority": `{"reprioritise":[{"id":0,"priority":3}]}`,
		"twice":        `{"reprioritise":[{"id":1,"priority":3},{"id":1,"priority":4}]}`,
	}
	for name, doc := range cases {
		parsed, err := taskspec.Parse(strings.NewReader(doc))
		if err == nil {
			_, err = taskspec.Validate(parsed)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPhasesCompileToABarrier(t *testing.T) {
	applied := parse(t, `{"phases":[
		{"name":"first","tasks":[
			{"title":"schema","workspace":"nixos"},
			{"title":"proto","workspace":"nixos"}
		]},
		{"name":"second","tasks":[
			{"title":"writer","workspace":"nixos"},
			{"title":"reader","workspace":"nixos"}
		]}
	]}`)

	if len(applied.Tasks) != 4 {
		t.Fatalf("tasks = %d, want 4", len(applied.Tasks))
	}
	// Every second-phase task waits for every first-phase task, and none of the
	// tasks inside a phase wait on each other: that is what makes it parallel.
	for _, task := range []int{2, 3} {
		if want := []int{0, 1}; !equal(applied.After[task], want) {
			t.Errorf("after[%d] = %v, want %v", task, applied.After[task], want)
		}
	}
	for _, task := range []int{0, 1} {
		if len(applied.After[task]) != 0 {
			t.Errorf("after[%d] = %v, want no dependency inside the phase", task, applied.After[task])
		}
	}
}

func TestPhaseAfterNamesAnotherPhase(t *testing.T) {
	applied := parse(t, `{"phases":[
		{"name":"schema","tasks":[{"title":"schema","workspace":"nixos"}]},
		{"name":"api","tasks":[{"title":"api","workspace":"nixos"}]},
		{"name":"docs","after":"schema","tasks":[{"title":"docs","workspace":"nixos"}]}
	]}`)

	// docs already waits on api because it is the previous phase, and on schema
	// because the phase names it. The duplicate is collapsed.
	if want := []int{0, 1}; !equal(applied.After[2], want) {
		t.Fatalf("after[2] = %v, want %v", applied.After[2], want)
	}

	// A phase-level edge alone can cross phases that are not adjacent.
	applied = parse(t, `{"phases":[
		{"name":"schema","tasks":[{"title":"schema","workspace":"nixos"}]},
		{"name":"api","tasks":[{"title":"api","workspace":"nixos"}]},
		{"name":"docs","after":"schema","tasks":[{"title":"docs","workspace":"nixos"}]},
		{"name":"release","after":"schema","tasks":[{"title":"release","workspace":"nixos"}]}
	]}`)
	if want := []int{0, 2}; !equal(applied.After[3], want) {
		t.Errorf("after[3] = %v, want %v", applied.After[3], want)
	}
}

func TestPhaseErrorsAreRefusedWhole(t *testing.T) {
	cases := map[string]string{
		"missing phase": `{"phases":[
			{"name":"one","tasks":[{"title":"a","workspace":"nixos"}]},
			{"name":"two","after":"nope","tasks":[{"title":"b","workspace":"nixos"}]}
		]}`,
		"cycle": `{"phases":[
			{"name":"one","after":"two","tasks":[{"title":"a","workspace":"nixos"}]},
			{"name":"two","tasks":[{"title":"b","workspace":"nixos"}]}
		]}`,
		"self phase": `{"phases":[
			{"name":"one","after":"one","tasks":[{"title":"a","workspace":"nixos"}]}
		]}`,
		"duplicate across phases": `{"phases":[
			{"name":"one","tasks":[{"title":"a","workspace":"nixos"}]},
			{"name":"two","tasks":[{"title":"a","workspace":"nixos"}]}
		]}`,
		"duplicate phase name": `{"phases":[
			{"name":"one","tasks":[{"title":"a","workspace":"nixos"}]},
			{"name":"one","tasks":[{"title":"b","workspace":"nixos"}]}
		]}`,
		"unknown phase field": `{"phases":[
			{"name":"one","owner":"me","tasks":[{"title":"a","workspace":"nixos"}]}
		]}`,
		"unknown task field": `{"phases":[
			{"name":"one","tasks":[{"title":"a","workspace":"nixos","assignee":"me"}]}
		]}`,
	}
	for name, doc := range cases {
		parsed, err := taskspec.Parse(strings.NewReader(doc))
		if err != nil {
			if !strings.Contains(name, "unknown") {
				t.Errorf("%s: parse: %v", name, err)
			}
			continue
		}
		if _, err := taskspec.Validate(parsed); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func equal(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
