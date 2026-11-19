package workflows_test

import (
	"strings"
	"testing"

	"loa/workflows"
)

func TestExampleWorkflowIsValid(t *testing.T) {
	workflow, err := workflows.Load("example.json")
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if workflow.Merge != workflows.MergeAuto {
		t.Errorf("merge = %q, want auto", workflow.Merge)
	}
	if workflow.Prompts["plan"] != "prompts/planner.md" {
		t.Errorf("plan prompt = %q", workflow.Prompts["plan"])
	}
	if len(workflow.Stages) != 3 || workflow.Stages[0] != "plan" {
		t.Errorf("stages = %v", workflow.Stages)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := workflows.Parse(strings.NewReader(`{
		"prompts": {"plan": "prompts/planner.md"},
		"check": "go test ./...",
		"merge": "auto",
		"stages": ["plan"],
		"cheks": "typo"
	}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want the unknown field named", err)
	}
}

func TestValidateRejectsIncompleteWorkflows(t *testing.T) {
	valid := workflows.Workflow{
		Prompts: map[string]string{"plan": "prompts/planner.md"},
		Check:   "go test ./...",
		Merge:   workflows.MergeAuto,
		Stages:  []string{"plan"},
	}
	if err := workflows.Validate(valid); err != nil {
		t.Fatalf("valid workflow: %v", err)
	}

	cases := map[string]workflows.Workflow{
		"no prompts":      {Check: valid.Check, Merge: valid.Merge, Stages: valid.Stages},
		"empty prompt":    {Prompts: map[string]string{"plan": ""}, Check: valid.Check, Merge: valid.Merge, Stages: valid.Stages},
		"no check":        {Prompts: valid.Prompts, Merge: workflows.MergeManual, Stages: valid.Stages},
		"auto no check":   {Prompts: valid.Prompts, Merge: workflows.MergeAuto, Stages: valid.Stages},
		"bad merge":       {Prompts: valid.Prompts, Check: valid.Check, Merge: "sometimes", Stages: valid.Stages},
		"no stages":       {Prompts: valid.Prompts, Check: valid.Check, Merge: valid.Merge},
		"empty stage":     {Prompts: valid.Prompts, Check: valid.Check, Merge: valid.Merge, Stages: []string{" "}},
		"duplicate stage": {Prompts: valid.Prompts, Check: valid.Check, Merge: valid.Merge, Stages: []string{"plan", "plan"}},
	}
	for name, workflow := range cases {
		if err := workflows.Validate(workflow); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadRejectsAMissingFile(t *testing.T) {
	if _, err := workflows.Load("does-not-exist.json"); err == nil {
		t.Fatal("accepted a missing file")
	}
}
