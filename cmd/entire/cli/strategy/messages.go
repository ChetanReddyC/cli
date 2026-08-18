package strategy

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

// MaxDescriptionLength is the maximum length for descriptions in commit messages
// before truncation occurs.
const MaxDescriptionLength = 60

// TruncateDescription truncates a string to maxLen runes, adding "..." if truncated.
// Uses rune-based slicing to avoid splitting multi-byte UTF-8 characters.
// If maxLen is less than 3, truncates without ellipsis.
func TruncateDescription(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	if maxLen < 3 {
		return stringutil.TruncateRunes(s, maxLen, "")
	}
	return stringutil.TruncateRunes(s, maxLen, "...")
}

// FormatSubagentEndMessage formats a commit message for when a subagent completes.
// Format: "Completed '<agent-type>' agent: <description> (<tool-use-id>)"
//
// Edge cases:
//   - Empty description: "Completed '<agent-type>' agent (<tool-use-id>)"
//   - Empty agentType: "Completed agent: <description> (<tool-use-id>)"
//   - Both empty: "Task: <tool-use-id>"
func FormatSubagentEndMessage(agentType, description, toolUseID string) string {
	return formatSubagentMessage("Completed", agentType, description, toolUseID)
}

// formatSubagentMessage is a shared helper for start/end messages.
func formatSubagentMessage(verb, agentType, description, toolUseID string) string {
	// Both empty - fall back to simple format
	if agentType == "" && description == "" {
		return "Task: " + toolUseID
	}

	// Truncate description if needed
	if description != "" {
		description = TruncateDescription(description, MaxDescriptionLength)
	}

	// Build message based on what fields are present
	if agentType != "" && description != "" {
		return fmt.Sprintf("%s '%s' agent: %s (%s)", verb, agentType, description, toolUseID)
	}
	if agentType != "" {
		return fmt.Sprintf("%s '%s' agent (%s)", verb, agentType, toolUseID)
	}
	// agentType is empty, description is present
	return fmt.Sprintf("%s agent: %s (%s)", verb, description, toolUseID)
}

// IncrementalTypeBackgroundProgress is the TaskStepContext.IncrementalType
// stamped on turn-end backstop snapshots of an in-flight background task
// (cli's captureInFlightTaskIncremental). It lives here, not in cli, because
// FormatIncrementalSubject is the single place that needs to match on it to
// pick a rendering — keep this the one definition callers stamp and compare
// against.
const IncrementalTypeBackgroundProgress = "background_progress"

// FormatIncrementalSubject formats the commit message subject for incremental
// checkpoints. incrementalType selects the rendering:
//   - IncrementalTypeBackgroundProgress: delegates to
//     FormatBackgroundProgressSubject, which renders subagentType/
//     taskDescription — this checkpoint has no todo content to fall back to.
//   - anything else (e.g. "TodoWrite", the post-todo incremental's tool
//     name): delegates to FormatIncrementalMessage, ignoring subagentType/
//     taskDescription — unchanged from before IncrementalTypeBackgroundProgress
//     existed.
func FormatIncrementalSubject(
	incrementalType string,
	subagentType string,
	taskDescription string,
	todoContent string,
	incrementalSequence int,
	shortToolUseID string,
) string {
	if incrementalType == IncrementalTypeBackgroundProgress {
		return FormatBackgroundProgressSubject(subagentType, taskDescription, shortToolUseID)
	}
	return FormatIncrementalMessage(todoContent, incrementalSequence, shortToolUseID)
}

// FormatBackgroundProgressSubject formats the commit message subject for a
// turn-end incremental snapshot of an in-flight background task. Unlike
// per-todo incrementals (FormatIncrementalMessage, driven by todo content),
// this checkpoint has no todo text to render — the description comes from
// the in-flight marker recorded at the task's launch time.
//
// Edge cases:
//   - Both present: "Background <subagent-type> task: <description> (<tool-use-id>)"
//   - Empty description: "Background <subagent-type> task (<tool-use-id>)"
//   - Empty subagentType: "Background task: <description> (<tool-use-id>)"
//   - Both empty: "Background task (<tool-use-id>)"
func FormatBackgroundProgressSubject(subagentType, taskDescription, shortToolUseID string) string {
	if taskDescription != "" {
		taskDescription = TruncateDescription(taskDescription, MaxDescriptionLength)
	}
	switch {
	case subagentType != "" && taskDescription != "":
		return fmt.Sprintf("Background %s task: %s (%s)", subagentType, taskDescription, shortToolUseID)
	case subagentType != "":
		return fmt.Sprintf("Background %s task (%s)", subagentType, shortToolUseID)
	case taskDescription != "":
		return fmt.Sprintf("Background task: %s (%s)", taskDescription, shortToolUseID)
	default:
		return fmt.Sprintf("Background task (%s)", shortToolUseID)
	}
}

// FormatIncrementalMessage formats a commit message for an incremental checkpoint.
// Format: "<todo-content> (<tool-use-id>)"
//
// If todoContent is empty, falls back to: "Checkpoint #<sequence>: <tool-use-id>"
func FormatIncrementalMessage(todoContent string, sequence int, toolUseID string) string {
	if todoContent == "" {
		return fmt.Sprintf("Checkpoint #%d: %s", sequence, toolUseID)
	}

	// Truncate todo content if needed
	todoContent = TruncateDescription(todoContent, MaxDescriptionLength)
	return fmt.Sprintf("%s (%s)", todoContent, toolUseID)
}

// todoItem represents a single item in the TodoWrite tool_input.todos array.
type todoItem struct {
	Content    string `json:"content"`
	ActiveForm string `json:"activeForm"`
	Status     string `json:"status"`
}

// ExtractLastCompletedTodo extracts the content of the last completed todo item from tool_input.
// This represents the work that was just finished and is used for commit messages.
//
// When TodoWrite is called in PostToolUse, the NEW list is provided which has the
// just-completed work marked as "completed". The last completed item is the most
// recently finished task.
//
// Returns empty string if no completed items exist or JSON is invalid.
func ExtractLastCompletedTodo(todosJSON []byte) string {
	if len(todosJSON) == 0 {
		return ""
	}

	var todos []todoItem
	if err := json.Unmarshal(todosJSON, &todos); err != nil {
		return ""
	}

	// Find the last completed item - this is the work that was just finished
	var lastCompleted string
	for _, todo := range todos {
		if todo.Status == "completed" {
			lastCompleted = todo.Content
		}
	}
	return lastCompleted
}

// CountTodos returns the number of todo items in the JSON array.
// Returns 0 if the JSON is invalid or empty.
func CountTodos(todosJSON []byte) int {
	if len(todosJSON) == 0 {
		return 0
	}

	var todos []todoItem
	if err := json.Unmarshal(todosJSON, &todos); err != nil {
		return 0
	}

	return len(todos)
}
