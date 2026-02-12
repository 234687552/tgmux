package tmux

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// ansiRegex 匹配 ANSI 转义序列
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\].*?\x07|\x1b\[.*?m`)

// CapturePaneRaw 捕获窗口原始内容（含 ANSI 转义）
func (m *Manager) CapturePaneRaw(windowID string) (string, error) {
	cmd := exec.Command("tmux", "capture-pane", "-t", m.target(windowID), "-p", "-e")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("capture-pane: %w", err)
	}
	return string(out), nil
}

// CapturePaneClean 捕获并清理 ANSI 转义序列
func (m *Manager) CapturePaneClean(windowID string) (string, error) {
	raw, err := m.CapturePaneRaw(windowID)
	if err != nil {
		return "", err
	}
	return StripANSI(raw), nil
}

// StripANSI 去除 ANSI 转义序列
func StripANSI(text string) string {
	return ansiRegex.ReplaceAllString(text, "")
}

// RenderScreenshot 将 tmux 窗口内容渲染为 PNG 图片
// 依赖外部工具: aha (ANSI -> HTML) + wkhtmltoimage (HTML -> PNG)
// 工具不可用时返回 error，调用方应降级为纯文本
func (m *Manager) RenderScreenshot(windowID string) ([]byte, error) {
	// 检查依赖
	if _, err := exec.LookPath("aha"); err != nil {
		return nil, fmt.Errorf("aha not installed: %w", err)
	}
	if _, err := exec.LookPath("wkhtmltoimage"); err != nil {
		return nil, fmt.Errorf("wkhtmltoimage not installed: %w", err)
	}

	// 捕获原始内容（含 ANSI）
	raw, err := m.CapturePaneRaw(windowID)
	if err != nil {
		return nil, err
	}

	// ANSI -> HTML (via aha)
	ahaCmd := exec.Command("aha", "--no-header")
	ahaCmd.Stdin = strings.NewReader(raw)
	html, err := ahaCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("aha: %w", err)
	}

	// 包裹完整 HTML
	fullHTML := fmt.Sprintf(`<!DOCTYPE html><html><head><style>body{background:#1e1e1e;color:#d4d4d4;font-family:monospace;font-size:14px;padding:16px;white-space:pre;}</style></head><body>%s</body></html>`, string(html))

	// HTML -> PNG (via wkhtmltoimage)
	imgCmd := exec.Command("wkhtmltoimage", "--quality", "90", "--width", "800", "-", "-")
	imgCmd.Stdin = strings.NewReader(fullHTML)
	png, err := imgCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("wkhtmltoimage: %w", err)
	}

	return png, nil
}
