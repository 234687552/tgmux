package tmux

import (
	"fmt"
	"os/exec"
	"strings"
)

const SessionName = "tgmux"

type WindowInfo struct {
	ID   string // e.g. "@0"
	Name string // e.g. "claude-my-project"
}

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

// EnsureSession 检查 tgmux session 是否存在，不存在则创建
func (m *Manager) EnsureSession() error {
	cmd := exec.Command("tmux", "has-session", "-t", SessionName)
	if err := cmd.Run(); err != nil {
		// session 不存在，创建一个
		cmd = exec.Command("tmux", "new-session", "-d", "-s", SessionName)
		return cmd.Run()
	}
	return nil
}

// NewWindow 创建新窗口，返回 window ID
func (m *Manager) NewWindow(name string) (string, error) {
	// 确保 session 存在（可能被销毁过）
	if err := m.EnsureSession(); err != nil {
		return "", fmt.Errorf("ensure session: %w", err)
	}
	cmd := exec.Command("tmux", "new-window", "-t", SessionName, "-n", name, "-P", "-F", "#{window_id}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("new-window: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// KillWindow 关闭窗口
func (m *Manager) KillWindow(windowID string) error {
	target := fmt.Sprintf("%s:%s", SessionName, windowID)
	cmd := exec.Command("tmux", "kill-window", "-t", target)
	return cmd.Run()
}

// target 返回 tmux target 格式
func (m *Manager) target(windowID string) string {
	return fmt.Sprintf("%s:%s", SessionName, windowID)
}

// SendKeys 发送单行文本（不含换行）
func (m *Manager) SendKeys(windowID string, text string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", m.target(windowID), "-l", text)
	return cmd.Run()
}

// SendEnter 发送回车
func (m *Manager) SendEnter(windowID string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", m.target(windowID), "Enter")
	return cmd.Run()
}

// SendEscape 发送 ESC
func (m *Manager) SendEscape(windowID string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", m.target(windowID), "Escape")
	return cmd.Run()
}

// SendSpecialKey 发送特殊键名（Up, Down, Left, Right, Space, Tab, C-c 等）
func (m *Manager) SendSpecialKey(windowID string, keyName string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", m.target(windowID), keyName)
	return cmd.Run()
}

// LoadBuffer 通过 stdin pipe 加载多行文本到 buffer，然后粘贴到窗口
func (m *Manager) LoadBuffer(windowID string, text string) error {
	// Step 1: load-buffer from stdin
	loadCmd := exec.Command("tmux", "load-buffer", "-")
	loadCmd.Stdin = strings.NewReader(text)
	if err := loadCmd.Run(); err != nil {
		return fmt.Errorf("load-buffer: %w", err)
	}
	// Step 2: paste-buffer to target window
	pasteCmd := exec.Command("tmux", "paste-buffer", "-t", m.target(windowID))
	if err := pasteCmd.Run(); err != nil {
		return fmt.Errorf("paste-buffer: %w", err)
	}
	return nil
}

// SendText 自动判断单行/多行，发送后追加 Enter
func (m *Manager) SendText(windowID string, text string) error {
	if strings.Contains(text, "\n") {
		// 多行文本用 load-buffer + paste-buffer
		if err := m.LoadBuffer(windowID, text); err != nil {
			return err
		}
	} else {
		// 单行用 send-keys -l
		if err := m.SendKeys(windowID, text); err != nil {
			return err
		}
	}
	return m.SendEnter(windowID)
}

// ListWindows 列出当前 session 中的所有窗口
func (m *Manager) ListWindows() ([]WindowInfo, error) {
	cmd := exec.Command("tmux", "list-windows", "-t", SessionName, "-F", "#{window_id}\t#{window_name}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list-windows: %w", err)
	}
	var windows []WindowInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			windows = append(windows, WindowInfo{ID: parts[0], Name: parts[1]})
		}
	}
	return windows, nil
}

// IsWindowAlive 检查窗口是否存在
func (m *Manager) IsWindowAlive(windowID string) bool {
	cmd := exec.Command("tmux", "list-windows", "-t", SessionName, "-F", "#{window_id}")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == windowID {
			return true
		}
	}
	return false
}

// PaneCommand 返回窗口当前 pane 运行的进程名（如 "node", "bash"）
func (m *Manager) PaneCommand(windowID string) string {
	cmd := exec.Command("tmux", "display-message", "-t", m.target(windowID), "-p", "#{pane_current_command}")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// IsBackendAlive 检查窗口中的后端进程是否还在运行（未回退到 shell）
func (m *Manager) IsBackendAlive(windowID string) bool {
	proc := m.PaneCommand(windowID)
	if proc == "" {
		return false
	}
	switch proc {
	case "bash", "zsh", "sh", "fish", "dash", "ksh", "csh", "tcsh":
		return false
	}
	return true
}

// SessionAlive 检查 tgmux session 是否存在
func (m *Manager) SessionAlive() bool {
	cmd := exec.Command("tmux", "has-session", "-t", SessionName)
	return cmd.Run() == nil
}
