package herdr_test

import (
	"testing"

	"loa/internal/herdr"
)

func TestTaskLabelRoundTrip(t *testing.T) {
	label := herdr.TaskLabel(7)
	if label != "loa #7" {
		t.Fatalf("TaskLabel(7) = %q", label)
	}
	id, ok := herdr.ParseTaskLabel(label)
	if !ok || id != 7 {
		t.Fatalf("ParseTaskLabel(%q) = %d, %v", label, id, ok)
	}
	if number := herdr.TaskLabelNumber(label); number != "7" {
		t.Fatalf("TaskLabelNumber(%q) = %q", label, number)
	}
}

// A workspace left by a binary from before the rename is still this runner's to
// reconcile, so its label must still parse.
func TestParseTaskLabelLegacyPrefix(t *testing.T) {
	id, ok := herdr.ParseTaskLabel("taskflow #7")
	if !ok || id != 7 {
		t.Fatalf("legacy label parsed as %d, %v", id, ok)
	}
	if number := herdr.TaskLabelNumber("taskflow #7"); number != "7" {
		t.Fatalf("TaskLabelNumber(legacy) = %q", number)
	}
}

func TestParseTaskLabelRejectsNonTaskLabels(t *testing.T) {
	for _, label := range []string{"", "nixos", "loa #", "loa #0", "loa #x", "loa 7", "tasks #7"} {
		if id, ok := herdr.ParseTaskLabel(label); ok {
			t.Errorf("ParseTaskLabel(%q) = %d, true", label, id)
		}
	}
}

func TestTaskLabelNumberKeepsUnknownLabel(t *testing.T) {
	if got := herdr.TaskLabelNumber("nixos"); got != "nixos" {
		t.Fatalf("TaskLabelNumber(nixos) = %q", got)
	}
}
