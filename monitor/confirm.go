package monitor

import (
	"strings"
)

// ConfirmPatterns are patterns that indicate a permission confirmation prompt
var ConfirmPatterns = []string{
	"Allow",
	"Deny",
	"(y/n)",
	"(Y/N)",
	"(y/N)",
	"(Y/n)",
	"Do you want to proceed",
	"Are you sure",
	"allow this",
	"approve this",
}

// DetectConfirmPrompt checks if the text contains a permission confirmation prompt
func DetectConfirmPrompt(text string) bool {
	lower := strings.ToLower(text)
	for _, pattern := range ConfirmPatterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// DetectClaudeToolUse checks if the text indicates a Claude tool_use that needs confirmation
func DetectClaudeToolUse(text string) bool {
	return strings.Contains(text, "[tool:") && (strings.Contains(text, "Allow") || strings.Contains(text, "allow"))
}

// InteractivePatterns are patterns that indicate an interactive UI (menus, selectors, etc.)
var InteractivePatterns = []string{
	"❯",           // Selector cursor
	"●",           // Selected radio button
	"○",           // Unselected radio button
	"◉",           // Filled circle selector
	"[ ]",         // Checkbox unchecked
	"[x]",         // Checkbox checked
	"[X]",         // Checkbox checked
	"Use arrow",   // Arrow key instruction
	"tab to cycle", // Tab instruction
	"Esc to cancel", // Escape instruction
}

// DetectInteractiveUI checks if the text contains an interactive UI that needs navigation
func DetectInteractiveUI(text string) bool {
	for _, pattern := range InteractivePatterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}
