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
