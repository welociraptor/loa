package herdr

import (
	"fmt"
	"strconv"
	"strings"
)

// taskLabelPrefix marks a workspace the runner created for one task.
const taskLabelPrefix = "taskflow #"

// TaskLabel is the workspace label a task's worktree carries. It is the one link
// between the queue and live session state that needs no stored id, which is why
// reconciliation falls back to it when a row has no workspace recorded.
func TaskLabel(taskID int64) string {
	return fmt.Sprintf("%s%d", taskLabelPrefix, taskID)
}

// ParseTaskLabel reads a task id back out of a workspace label.
func ParseTaskLabel(label string) (int64, bool) {
	rest, ok := strings.CutPrefix(label, taskLabelPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
