package herdr

import (
	"fmt"
	"strconv"
	"strings"
)

// taskLabelPrefix marks a workspace the runner created for one task.
const taskLabelPrefix = "loa #"

// labelPrefixes are the prefixes a task label may carry. The second is what the
// runner wrote before the rename; reconciliation still accepts it so a workspace
// left behind by an older binary is not orphaned.
var labelPrefixes = []string{taskLabelPrefix, "taskflow #"}

func trimTaskLabel(label string) (string, bool) {
	for _, prefix := range labelPrefixes {
		if rest, ok := strings.CutPrefix(label, prefix); ok {
			return rest, true
		}
	}
	return "", false
}

// TaskLabel is the workspace label a task's worktree carries. It is the one link
// between the queue and live session state that needs no stored id, which is why
// reconciliation falls back to it when a row has no workspace recorded.
func TaskLabel(taskID int64) string {
	return fmt.Sprintf("%s%d", taskLabelPrefix, taskID)
}

// ParseTaskLabel reads a task id back out of a workspace label.
func ParseTaskLabel(label string) (int64, bool) {
	rest, ok := trimTaskLabel(label)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// TaskLabelNumber is the id portion of a task label, for display. It returns the
// label unchanged when it carries no known prefix.
func TaskLabelNumber(label string) string {
	rest, ok := trimTaskLabel(label)
	if !ok {
		return label
	}
	return rest
}
