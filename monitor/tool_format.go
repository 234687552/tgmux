package monitor

import (
	"fmt"
	"strings"
)

const maxSummaryLen = 200

// FormatToolUseSummary formats a tool_use block into a brief summary line.
// e.g. "Read(src/main.go)", "Bash(go build ./...)"
func FormatToolUseSummary(name string, input map[string]interface{}) string {
	if input == nil {
		return name
	}

	var summary string
	switch name {
	case "Read", "Glob":
		summary = strVal(input, "file_path")
		if summary == "" {
			summary = strVal(input, "pattern")
		}
	case "Write":
		summary = strVal(input, "file_path")
	case "Edit", "NotebookEdit":
		summary = strVal(input, "file_path")
		if summary == "" {
			summary = strVal(input, "notebook_path")
		}
	case "Bash":
		summary = strVal(input, "command")
	case "Grep":
		summary = strVal(input, "pattern")
	case "Task":
		summary = strVal(input, "description")
	case "WebFetch":
		summary = strVal(input, "url")
	case "WebSearch":
		summary = strVal(input, "query")
	case "TodoWrite":
		if todos, ok := input["todos"].([]interface{}); ok {
			summary = fmt.Sprintf("%d item(s)", len(todos))
		}
	case "Skill":
		summary = strVal(input, "skill")
	default:
		// first non-empty string value
		for _, v := range input {
			if s, ok := v.(string); ok && s != "" {
				summary = s
				break
			}
		}
	}

	if summary == "" {
		return name
	}
	if len(summary) > maxSummaryLen {
		summary = summary[:maxSummaryLen] + "…"
	}
	return fmt.Sprintf("%s(%s)", name, summary)
}

// FormatToolResultStats formats tool result text into a stats summary.
func FormatToolResultStats(text string, toolName string) string {
	if text == "" {
		return ""
	}
	lines := countLines(text)

	switch toolName {
	case "Read":
		return fmt.Sprintf("  ⎿  Read %d lines", lines)
	case "Write":
		return fmt.Sprintf("  ⎿  Wrote %d lines", lines)
	case "Bash":
		return fmt.Sprintf("  ⎿  Output %d lines", lines)
	case "Grep":
		matches := countNonEmpty(text)
		return fmt.Sprintf("  ⎿  Found %d matches", matches)
	case "Glob":
		files := countNonEmpty(text)
		return fmt.Sprintf("  ⎿  Found %d files", files)
	case "Edit", "NotebookEdit":
		return "  ⎿  Edited"
	default:
		return fmt.Sprintf("  ⎿  %d lines", lines)
	}
}

// FormatEditDiff generates a simple diff summary between old and new strings.
func FormatEditDiff(oldString, newString string) string {
	if oldString == "" && newString == "" {
		return ""
	}
	oldLines := strings.Split(oldString, "\n")
	newLines := strings.Split(newString, "\n")

	// Simple line-level diff: count added/removed
	oldSet := make(map[string]int)
	for _, l := range oldLines {
		oldSet[l]++
	}
	newSet := make(map[string]int)
	for _, l := range newLines {
		newSet[l]++
	}

	added := 0
	for _, l := range newLines {
		if oldSet[l] > 0 {
			oldSet[l]--
		} else {
			added++
		}
	}
	// Reset oldSet
	oldSet2 := make(map[string]int)
	for _, l := range oldLines {
		oldSet2[l]++
	}
	removed := 0
	for _, l := range oldLines {
		if newSet[l] > 0 {
			newSet[l]--
		} else {
			removed++
		}
	}

	return fmt.Sprintf("  ⎿  +%d/-%d lines", added, removed)
}

func strVal(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func countNonEmpty(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
