package bot

import (
	"fmt"

	"github.com/go-telegram/bot/models"
)

// SessionInfo 用于会话列表展示
type SessionInfo struct {
	WindowID    string
	DisplayName string
	BoundTopic  string // 如果已绑定，显示 topic key；否则为空
}

// BackendKeyboard 后端选择键盘
func BackendKeyboard() models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "claude", CallbackData: "backend:claude"},
				{Text: "codex", CallbackData: "backend:codex"},
				{Text: "gemini", CallbackData: "backend:gemini"},
				{Text: "bash", CallbackData: "backend:bash"},
			},
		},
	}
}

// DirKeyboard 目录选择键盘
func DirKeyboard(favorites []string, recent []string) models.InlineKeyboardMarkup {
	var rows [][]models.InlineKeyboardButton

	// 收藏目录
	for _, dir := range favorites {
		short := shortenPath(dir)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("⭐ %s", short), CallbackData: fmt.Sprintf("dir:%s", dir)},
		})
	}

	// 最近使用（去重收藏）
	favSet := make(map[string]bool)
	for _, f := range favorites {
		favSet[f] = true
	}
	for _, dir := range recent {
		if favSet[dir] {
			continue
		}
		short := shortenPath(dir)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("🕐 %s", short), CallbackData: fmt.Sprintf("dir:%s", dir)},
		})
	}

	// 输入路径按钮
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "📁 输入路径...", CallbackData: "dir_input"},
	})

	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// ConfirmKeyboard 权限确认键盘
func ConfirmKeyboard(windowID string) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "✅ Yes", CallbackData: fmt.Sprintf("confirm:yes:%s", windowID)},
				{Text: "❌ No", CallbackData: fmt.Sprintf("confirm:no:%s", windowID)},
				{Text: "🔓 Always", CallbackData: fmt.Sprintf("confirm:always:%s", windowID)},
			},
		},
	}
}

// ScreenshotKeyboard 截图控制键盘
func ScreenshotKeyboard(windowID string) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "⬆", CallbackData: fmt.Sprintf("ss:up:%s", windowID)},
				{Text: "⬇", CallbackData: fmt.Sprintf("ss:down:%s", windowID)},
				{Text: "⬅", CallbackData: fmt.Sprintf("ss:left:%s", windowID)},
				{Text: "➡", CallbackData: fmt.Sprintf("ss:right:%s", windowID)},
			},
			{
				{Text: "Enter", CallbackData: fmt.Sprintf("ss:enter:%s", windowID)},
				{Text: "Esc", CallbackData: fmt.Sprintf("ss:esc:%s", windowID)},
				{Text: "Tab", CallbackData: fmt.Sprintf("ss:tab:%s", windowID)},
				{Text: "Space", CallbackData: fmt.Sprintf("ss:space:%s", windowID)},
			},
			{
				{Text: "Ctrl-C", CallbackData: fmt.Sprintf("ss:ctrlc:%s", windowID)},
				{Text: "y", CallbackData: fmt.Sprintf("ss:y:%s", windowID)},
				{Text: "n", CallbackData: fmt.Sprintf("ss:n:%s", windowID)},
				{Text: "🔄 Refresh", CallbackData: fmt.Sprintf("ss:refresh:%s", windowID)},
			},
		},
	}
}

// InteractiveKeyboard 交互式界面导航键盘
func InteractiveKeyboard(windowID string) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "⬆ Up", CallbackData: fmt.Sprintf("nav:up:%s", windowID)},
				{Text: "⬇ Down", CallbackData: fmt.Sprintf("nav:down:%s", windowID)},
				{Text: "⬅ Left", CallbackData: fmt.Sprintf("nav:left:%s", windowID)},
				{Text: "➡ Right", CallbackData: fmt.Sprintf("nav:right:%s", windowID)},
			},
			{
				{Text: "Space", CallbackData: fmt.Sprintf("nav:space:%s", windowID)},
				{Text: "Tab", CallbackData: fmt.Sprintf("nav:tab:%s", windowID)},
				{Text: "Enter", CallbackData: fmt.Sprintf("nav:enter:%s", windowID)},
			},
			{
				{Text: "Esc", CallbackData: fmt.Sprintf("nav:esc:%s", windowID)},
				{Text: "🔄 Refresh", CallbackData: fmt.Sprintf("nav:refresh:%s", windowID)},
			},
			{
				{Text: "✅ Yes", CallbackData: fmt.Sprintf("confirm:yes:%s", windowID)},
				{Text: "❌ No", CallbackData: fmt.Sprintf("confirm:no:%s", windowID)},
				{Text: "🔓 Always", CallbackData: fmt.Sprintf("confirm:always:%s", windowID)},
			},
		},
	}
}

// SessionListKeyboard 会话列表键盘（含绑定/关闭按钮）
func SessionListKeyboard(sessions []SessionInfo) models.InlineKeyboardMarkup {
	var rows [][]models.InlineKeyboardButton
	for _, s := range sessions {
		if s.BoundTopic != "" {
			// 已绑定：显示名 + [关闭]
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: fmt.Sprintf("🔗 %s", s.DisplayName), CallbackData: "noop"},
				{Text: "❌ 关闭", CallbackData: fmt.Sprintf("kill:%s", s.WindowID)},
			})
		} else {
			// 未绑定：显示名 + [绑定]
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: fmt.Sprintf("💤 %s", s.DisplayName), CallbackData: "noop"},
				{Text: "🔗 绑定", CallbackData: fmt.Sprintf("bind:%s", s.WindowID)},
			})
		}
	}
	// 新建会话按钮
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "➕ 新建会话", CallbackData: "new_session"},
	})
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// BrowseDirKeyboard 目录浏览键盘
func BrowseDirKeyboard(currentPath string, entries []DirEntry) models.InlineKeyboardMarkup {
	var rows [][]models.InlineKeyboardButton
	for _, entry := range entries {
		fullPath := fmt.Sprintf("%s/%s", currentPath, entry.Name)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("📂 %s", entry.Name), CallbackData: fmt.Sprintf("browse:%s", fullPath)},
			{Text: "⭐", CallbackData: fmt.Sprintf("fav:%s", fullPath)},
		})
	}
	// 选择当前目录
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "✅ 选择此目录", CallbackData: fmt.Sprintf("dir:%s", currentPath)},
	})
	// 返回上级
	if currentPath != "/" {
		parent := parentDir(currentPath)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "⬆️ 返回上级", CallbackData: fmt.Sprintf("browse:%s", parent)},
		})
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// DirEntry 目录条目
type DirEntry struct {
	Name  string
	IsDir bool
}

// shortenPath 缩短路径显示
func shortenPath(path string) string {
	if len(path) <= 40 {
		return path
	}
	// 替换 home 目录为 ~
	home := "~"
	_ = home
	return "..." + path[len(path)-37:]
}

// parentDir 获取父目录
func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' && i > 0 {
			return path[:i]
		}
	}
	return "/"
}
