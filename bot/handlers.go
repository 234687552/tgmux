package bot

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/user/tgmux/backend"
	"github.com/user/tgmux/state"
)

// defaultHandler 处理非命令的文本消息（也接收未匹配的 /命令，会自动转发到 tmux）
func (b *Bot) defaultHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}

	// 论坛话题关闭事件 - 自动清理关联的 tmux 窗口
	if update.Message.ForumTopicClosed != nil {
		b.handleTopicClosed(ctx, update.Message)
		return
	}

	if update.Message.Text == "" {
		return
	}
	msg := update.Message
	key := topicKeyFromMessage(msg)
	text := msg.Text

	// 检查状态机
	ts := b.getOrCreateState(key)
	switch ts.Phase {
	case "awaiting_path_input":
		// 用户输入了路径
		path := strings.TrimSpace(text)
		if path == "" {
			b.sendReply(ctx, msg, "路径不能为空，请重新输入：")
			return
		}
		// 展开 ~
		if strings.HasPrefix(path, "~/") {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, path[2:])
		}
		// 校验路径存在
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			b.sendReply(ctx, msg, fmt.Sprintf("目录不存在: %s\n请重新输入：", path))
			return
		}
		ts.SelectedDir = path
		b.setPhase(key, "awaiting_backend")
		kb := BackendKeyboard()
		b.sendReplyWithKeyboard(ctx, msg, "🚀 选择启动命令：", kb)
		return

	case "awaiting_dir":
		b.sendReply(ctx, msg, "请点击按钮选择目录，或点击 [📁 输入路径...] 手动输入")
		return

	case "awaiting_backend":
		b.sendReply(ctx, msg, "请点击按钮选择后端")
		return
	}

	slog.Info("defaultHandler", "key", key, "phase", ts.Phase, "text", text[:min(len(text), 30)])

	// 查绑定
	binding, ok := b.store.GetBinding(key)
	if ok {
		// 已绑定 - 检查窗口是否存活
		if !b.tmux.IsWindowAlive(binding.WindowID) {
			// 窗口已死 - 自动解绑
			b.unbind(key, binding)
			slog.Info("window dead, auto unbinding", "key", key, "window", binding.WindowID)
			b.sendReply(ctx, msg, "⚠️ 会话已断开，已自动解绑")
			b.handleUnbound(ctx, msg, key)
			return
		}
		// 窗口存活但后端进程可能已退出（回到 shell）
		if !b.tmux.IsBackendAlive(binding.WindowID) {
			b.unbind(key, binding)
			slog.Info("backend exited, auto unbinding", "key", key, "window", binding.WindowID)
			b.sendReply(ctx, msg, "⚠️ 后端进程已退出，已自动解绑")
			b.handleUnbound(ctx, msg, key)
			return
		}
		// ! 前缀：直接发送 bash 命令到 tmux pane（绕过 AI 后端输入队列）
		if strings.HasPrefix(text, "!") && len(text) > 1 {
			cmdText := strings.TrimSpace(text[1:])
			if err := b.tmux.SendKeys(binding.WindowID, cmdText); err != nil {
				b.sendReply(ctx, msg, fmt.Sprintf("发送命令失败: %v", err))
				return
			}
			b.tmux.SendEnter(binding.WindowID)
			return
		}

		// 窗口和后端都存活 - 转发消息到 tmux
		ch := b.getOrCreateSendChan(binding.WindowID)
		ch <- text
		return
	}

	// 未绑定
	b.handleUnbound(ctx, msg, key)
}

// handleUnbound 处理未绑定 topic 的消息
func (b *Bot) handleUnbound(ctx context.Context, msg *models.Message, key string) {
	windows, err := b.tmux.ListWindows()
	if err != nil || len(windows) == 0 {
		// 无已有窗口 - 直接进入 /new 流程
		b.startNewFlow(ctx, msg, key)
		return
	}

	// 有已有窗口 - 展示列表
	allBindings := b.store.AllBindings()
	boundWindows := make(map[string]string) // windowID -> topicKey
	for tk, bd := range allBindings {
		boundWindows[bd.WindowID] = tk
	}

	var sessions []SessionInfo
	for _, w := range windows {
		si := SessionInfo{
			WindowID:    w.ID,
			DisplayName: w.Name,
		}
		if tk, ok := boundWindows[w.ID]; ok {
			si.BoundTopic = tk
		}
		sessions = append(sessions, si)
	}

	kb := SessionListKeyboard(sessions)
	b.sendReplyWithKeyboard(ctx, msg, "该 Topic 尚未绑定会话，请选择：", kb)
}

// startNewFlow 进入 /new 两步创建流程
func (b *Bot) startNewFlow(ctx context.Context, msg *models.Message, key string) {
	b.setPhase(key, "awaiting_dir")
	dirs := b.store.GetDirs()
	kb := DirKeyboard(dirs.Favorites, dirs.Recent)
	b.sendReplyWithKeyboard(ctx, msg, "📂 选择项目目录：", kb)
}

// handleNew /new 命令
func (b *Bot) handleNew(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	key := topicKeyFromMessage(update.Message)
	b.startNewFlow(ctx, update.Message, key)
}

// handleSession /session 命令
func (b *Bot) handleSession(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	key := topicKeyFromMessage(msg)
	text := strings.TrimSpace(strings.TrimPrefix(msg.Text, "/session"))

	if text == "list" || text == " list" {
		// 列出所有窗口
		windows, err := b.tmux.ListWindows()
		if err != nil {
			b.sendReply(ctx, msg, fmt.Sprintf("获取窗口列表失败: %v", err))
			return
		}
		if len(windows) == 0 {
			b.sendReply(ctx, msg, "🖥 当前没有 tmux 窗口")
			return
		}
		allBindings := b.store.AllBindings()
		boundWindows := make(map[string]string)
		for tk, bd := range allBindings {
			boundWindows[bd.WindowID] = tk
		}
		var lines []string
		lines = append(lines, "🖥 所有 tmux 窗口\n")
		for _, w := range windows {
			if tk, ok := boundWindows[w.ID]; ok {
				lines = append(lines, fmt.Sprintf("%s  %s  ← 已绑定 %s", w.ID, w.Name, tk))
			} else {
				lines = append(lines, fmt.Sprintf("%s  %s  ← 未绑定", w.ID, w.Name))
			}
		}
		b.sendReply(ctx, msg, strings.Join(lines, "\n"))
		return
	}

	// 默认：显示当前绑定详情
	binding, ok := b.store.GetBinding(key)
	if !ok {
		b.sendReply(ctx, msg, "当前 Topic 尚未绑定会话\n使用 /new 创建新会话")
		return
	}
	alive := "运行中"
	if !b.tmux.IsWindowAlive(binding.WindowID) {
		alive = "已断开"
	}
	ago := time.Since(binding.CreatedAt).Truncate(time.Minute)
	reply := fmt.Sprintf("📋 当前会话信息\n├─ 窗口:    %s\n├─ 后端:    %s\n├─ 目录:    %s\n├─ 状态:    %s\n└─ 创建于:  %s ago",
		binding.WindowID, binding.Backend, binding.ProjectPath, alive, ago)
	b.sendReply(ctx, msg, reply)
}

// handleKill /kill 命令
func (b *Bot) handleKill(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	key := topicKeyFromMessage(msg)
	binding, ok := b.store.GetBinding(key)
	if !ok {
		b.sendReply(ctx, msg, "当前 Topic 尚未绑定会话")
		return
	}
	// 关闭窗口
	b.tmux.KillWindow(binding.WindowID)
	b.unbind(key, binding)
	b.sendReply(ctx, msg, fmt.Sprintf("✅ 已关闭会话 %s", binding.DisplayName))
}

// handleEsc /esc 命令
func (b *Bot) handleEsc(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	key := topicKeyFromMessage(msg)
	binding, ok := b.store.GetBinding(key)
	if !ok {
		b.sendReply(ctx, msg, "当前 Topic 尚未绑定会话")
		return
	}
	b.tmux.SendEscape(binding.WindowID)
	b.sendReply(ctx, msg, "⎋ 已发送 Escape")
}

// handleEnter /enter 命令
func (b *Bot) handleEnter(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	key := topicKeyFromMessage(msg)
	binding, ok := b.store.GetBinding(key)
	if !ok {
		b.sendReply(ctx, msg, "当前 Topic 尚未绑定会话")
		return
	}
	b.tmux.SendEnter(binding.WindowID)
}

// handleScreenshot /screenshot 命令
func (b *Bot) handleScreenshot(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	key := topicKeyFromMessage(msg)
	binding, ok := b.store.GetBinding(key)
	if !ok {
		b.sendReply(ctx, msg, "当前 Topic 尚未绑定会话")
		return
	}

	b.sendScreenshotToChat(ctx, msg.Chat.ID, msg.MessageThreadID, binding.WindowID)
}

// sendScreenshotToChat 截图并发送到 chat，附带控制键盘
func (b *Bot) sendScreenshotToChat(ctx context.Context, chatID int64, threadID int, windowID string) {
	kb := ScreenshotKeyboard(windowID)

	// 尝试渲染截图
	png, err := b.tmux.RenderScreenshot(windowID)
	if err != nil {
		// 降级为纯文本
		slog.Warn("screenshot render failed, fallback to text", "error", err)
		text, err2 := b.tmux.CapturePaneClean(windowID)
		if err2 != nil {
			return
		}
		if len(text) > 4000 {
			text = text[len(text)-4000:]
		}
		params := &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        fmt.Sprintf("```\n%s\n```", text),
			ReplyMarkup: kb,
		}
		if threadID != 0 {
			params.MessageThreadID = threadID
		}
		b.bot.SendMessage(ctx, params)
		return
	}

	// 发送图片
	params := &bot.SendPhotoParams{
		ChatID:      chatID,
		Photo:       &models.InputFileUpload{Filename: "screenshot.png", Data: bytes.NewReader(png)},
		ReplyMarkup: kb,
	}
	if threadID != 0 {
		params.MessageThreadID = threadID
	}
	b.bot.SendPhoto(ctx, params)
}

// handleCmd /cmd 命令
func (b *Bot) handleCmd(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	key := topicKeyFromMessage(msg)
	binding, ok := b.store.GetBinding(key)
	if !ok {
		b.sendReply(ctx, msg, "当前 Topic 尚未绑定会话")
		return
	}
	// 提取 /cmd 后的参数
	arg := strings.TrimSpace(strings.TrimPrefix(msg.Text, "/cmd"))
	if arg == "" {
		b.sendReply(ctx, msg, "用法: /cmd <命令>\n例如: /cmd config")
		return
	}
	// 发送为后端原生命令
	cmdText := "/" + arg
	ch := b.getOrCreateSendChan(binding.WindowID)
	ch <- cmdText
}

// handleDir /dir 命令
func (b *Bot) handleDir(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	text := strings.TrimSpace(strings.TrimPrefix(msg.Text, "/dir"))

	if strings.HasPrefix(text, "add ") {
		path := strings.TrimSpace(strings.TrimPrefix(text, "add "))
		if path == "" {
			b.sendReply(ctx, msg, "用法: /dir add <路径>")
			return
		}
		b.store.AddFavorite(expandHome(path))
		b.sendReply(ctx, msg, fmt.Sprintf("⭐ 已收藏: %s", path))
		return
	}

	if strings.HasPrefix(text, "rm ") {
		path := strings.TrimSpace(strings.TrimPrefix(text, "rm "))
		if path == "" {
			b.sendReply(ctx, msg, "用法: /dir rm <路径>")
			return
		}
		b.store.RemoveFavorite(expandHome(path))
		b.sendReply(ctx, msg, fmt.Sprintf("🗑 已移除收藏: %s", path))
		return
	}

	if strings.HasPrefix(text, "browse") {
		path := strings.TrimSpace(strings.TrimPrefix(text, "browse"))
		if path == "" {
			path, _ = os.UserHomeDir()
		}
		path = expandHome(path)
		entries, err := listSubDirs(path)
		if err != nil {
			b.sendReply(ctx, msg, fmt.Sprintf("浏览失败: %v", err))
			return
		}
		kb := BrowseDirKeyboard(path, entries)
		b.sendReplyWithKeyboard(ctx, msg, fmt.Sprintf("📂 %s", path), kb)
		return
	}

	// 默认：列出收藏+最近
	dirs := b.store.GetDirs()
	var lines []string
	lines = append(lines, "📂 目录管理\n")
	if len(dirs.Favorites) > 0 {
		lines = append(lines, "⭐ 收藏:")
		for _, f := range dirs.Favorites {
			lines = append(lines, "  "+f)
		}
	}
	if len(dirs.Recent) > 0 {
		lines = append(lines, "\n🕐 最近使用:")
		for _, r := range dirs.Recent {
			lines = append(lines, "  "+r)
		}
	}
	if len(dirs.Favorites) == 0 && len(dirs.Recent) == 0 {
		lines = append(lines, "暂无目录记录\n使用 /dir add <路径> 添加收藏\n使用 /dir browse 浏览目录")
	}
	b.sendReply(ctx, msg, strings.Join(lines, "\n"))
}

// handleCallback 处理内联键盘回调
func (b *Bot) handleCallback(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}
	cq := update.CallbackQuery
	key := topicKeyFromCallback(cq)
	if key == "" {
		slog.Warn("callback: empty key, ignoring", "data", cq.Data)
		return
	}
	data := cq.Data
	slog.Info("handleCallback", "key", key, "data", data)

	// Answer callback 消除加载状态
	tgBot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID})

	// 获取原始消息用于回复
	var chatID int64
	var threadID int
	if msg := cq.Message.Message; msg != nil {
		chatID = msg.Chat.ID
		threadID = msg.MessageThreadID
	}

	switch {
	case strings.HasPrefix(data, "backend:"):
		backendType := backend.Type(strings.TrimPrefix(data, "backend:"))
		b.createSession(ctx, key, chatID, threadID, backendType)

	case strings.HasPrefix(data, "dir:"):
		dirPath := strings.TrimPrefix(data, "dir:")
		ts := b.getOrCreateState(key)
		ts.SelectedDir = dirPath
		b.setPhase(key, "awaiting_backend")
		kb := BackendKeyboard()
		b.sendMsg(ctx, chatID, threadID, "🚀 选择启动命令：", &kb)

	case data == "dir_input":
		b.setPhase(key, "awaiting_path_input")
		b.sendMsg(ctx, chatID, threadID, "请输入项目目录的完整路径：", nil)

	case strings.HasPrefix(data, "bind:"):
		windowID := strings.TrimPrefix(data, "bind:")
		b.bindExisting(ctx, key, chatID, threadID, windowID)

	case data == "new_session":
		b.setPhase(key, "awaiting_dir")
		dirs := b.store.GetDirs()
		kb := DirKeyboard(dirs.Favorites, dirs.Recent)
		b.sendMsg(ctx, chatID, threadID, "📂 选择项目目录：", &kb)

	case strings.HasPrefix(data, "confirm:"):
		parts := strings.SplitN(strings.TrimPrefix(data, "confirm:"), ":", 2)
		if len(parts) == 2 {
			b.handleConfirm(ctx, key, parts[1], parts[0])
		}

	case strings.HasPrefix(data, "browse:"):
		dirPath := strings.TrimPrefix(data, "browse:")
		entries, err := listSubDirs(dirPath)
		if err != nil {
			return
		}
		kb := BrowseDirKeyboard(dirPath, entries)
		b.sendMsg(ctx, chatID, threadID, fmt.Sprintf("📂 %s", dirPath), &kb)

	case strings.HasPrefix(data, "fav:"):
		dirPath := strings.TrimPrefix(data, "fav:")
		b.store.AddFavorite(dirPath)
		b.sendMsg(ctx, chatID, threadID, fmt.Sprintf("⭐ 已收藏: %s", dirPath), nil)

	case strings.HasPrefix(data, "kill:"):
		windowID := strings.TrimPrefix(data, "kill:")
		b.tmux.KillWindow(windowID)
		// 清理所有绑定到该窗口的 binding
		for tk, bd := range b.store.AllBindings() {
			if bd.WindowID == windowID {
				b.unbind(tk, bd)
			}
		}
		b.sendMsg(ctx, chatID, threadID, "✅ 已关闭窗口", nil)

	case strings.HasPrefix(data, "ss:"):
		// 截图控制键盘回调
		parts := strings.SplitN(strings.TrimPrefix(data, "ss:"), ":", 2)
		if len(parts) == 2 {
			b.handleScreenshotAction(ctx, chatID, threadID, parts[0], parts[1])
		}

	case strings.HasPrefix(data, "nav:"):
		// 交互式导航键盘回调
		parts := strings.SplitN(strings.TrimPrefix(data, "nav:"), ":", 2)
		if len(parts) == 2 {
			b.handleNavAction(ctx, chatID, threadID, parts[0], parts[1])
		}
	}
}

// createSession 创建新会话
func (b *Bot) createSession(ctx context.Context, key string, chatID int64, threadID int, backendType backend.Type) {
	ts := b.getOrCreateState(key)
	if ts.SelectedDir == "" {
		b.sendMsg(ctx, chatID, threadID, "错误：未选择目录", nil)
		return
	}

	be := backend.Get(backendType, b.cfg)
	dirName := filepath.Base(ts.SelectedDir)
	windowName := fmt.Sprintf("%s-%s", backendType, dirName)

	// 创建 tmux 窗口
	windowID, err := b.tmux.NewWindow(windowName)
	if err != nil {
		b.sendMsg(ctx, chatID, threadID, fmt.Sprintf("创建窗口失败: %v", err), nil)
		return
	}

	// cd 到项目目录
	b.tmux.SendKeys(windowID, fmt.Sprintf("cd %s", ts.SelectedDir))
	b.tmux.SendEnter(windowID)

	// 清理可能阻止嵌套启动的环境变量
	b.tmux.SendKeys(windowID, "unset CLAUDECODE CLAUDE_CODE 2>/dev/null; true")
	b.tmux.SendEnter(windowID)

	// 启动后端命令（bash 跳过）
	if backendType != backend.TypeBash && be.Command != "" {
		time.Sleep(500 * time.Millisecond) // 等待 cd + unset 完成
		cmd := be.Command
		if len(be.Args) > 0 {
			cmd += " " + strings.Join(be.Args, " ")
		}
		b.tmux.SendKeys(windowID, cmd)
		b.tmux.SendEnter(windowID)
	}

	// 设置绑定
	binding := state.Binding{
		WindowID:    windowID,
		Backend:     string(backendType),
		ProjectPath: ts.SelectedDir,
		DisplayName: fmt.Sprintf("%s @ %s", backendType, dirName),
		CreatedAt:   time.Now(),
		Status:      "running",
	}
	b.store.SetBinding(key, binding)
	b.store.AddRecent(ts.SelectedDir)

	// 初始化串行发送 channel
	b.getOrCreateSendChan(windowID)

	// 启动输出监控
	b.StartMonitorForBinding(ctx, key, binding, chatID, threadID)

	// 重置状态机
	b.setPhase(key, "bound")

	b.sendMsg(ctx, chatID, threadID, fmt.Sprintf("✅ 已创建 %s 会话 @ %s", backendType, ts.SelectedDir), nil)
	slog.Info("session created", "key", key, "backend", backendType, "dir", ts.SelectedDir, "window", windowID)
}

// bindExisting 绑定已有窗口
func (b *Bot) bindExisting(ctx context.Context, key string, chatID int64, threadID int, windowID string) {
	// 检查后端是否还在运行
	if !b.tmux.IsBackendAlive(windowID) {
		b.sendMsg(ctx, chatID, threadID, "⚠️ 该窗口的后端进程已退出，无法绑定", nil)
		return
	}

	// 查找窗口信息
	windows, _ := b.tmux.ListWindows()
	var windowName string
	for _, w := range windows {
		if w.ID == windowID {
			windowName = w.Name
			break
		}
	}

	binding := state.Binding{
		WindowID:    windowID,
		Backend:     "unknown",
		ProjectPath: "",
		DisplayName: windowName,
		CreatedAt:   time.Now(),
		Status:      "running",
	}
	b.store.SetBinding(key, binding)
	b.getOrCreateSendChan(windowID)

	// 启动输出监控
	b.StartMonitorForBinding(ctx, key, binding, chatID, threadID)

	b.setPhase(key, "bound")

	b.sendMsg(ctx, chatID, threadID, fmt.Sprintf("🔗 已绑定到窗口 %s (%s)", windowID, windowName), nil)
}

// handleConfirm 处理权限确认
func (b *Bot) handleConfirm(ctx context.Context, key string, windowID string, action string) {
	switch action {
	case "yes":
		b.tmux.SendKeys(windowID, "y")
		b.tmux.SendEnter(windowID)
	case "no":
		b.tmux.SendKeys(windowID, "n")
		b.tmux.SendEnter(windowID)
	case "always":
		// 发送 always allow（具体命令因后端而异）
		b.tmux.SendKeys(windowID, "!")
		b.tmux.SendEnter(windowID)
	}
}

// handleTopicClosed 论坛话题关闭时自动清理
func (b *Bot) handleTopicClosed(ctx context.Context, msg *models.Message) {
	key := topicKeyFromMessage(msg)
	binding, ok := b.store.GetBinding(key)
	if !ok {
		return
	}
	slog.Info("topic closed, auto cleanup", "key", key, "window", binding.WindowID)
	b.tmux.KillWindow(binding.WindowID)
	b.unbind(key, binding)
}

// specialKeyMap maps callback action names to tmux key names
var specialKeyMap = map[string]string{
	"up":    "Up",
	"down":  "Down",
	"left":  "Left",
	"right": "Right",
	"enter": "Enter",
	"esc":   "Escape",
	"tab":   "Tab",
	"space": "Space",
	"ctrlc": "C-c",
}

// handleScreenshotAction 处理截图控制键盘按钮
func (b *Bot) handleScreenshotAction(ctx context.Context, chatID int64, threadID int, action string, windowID string) {
	if action == "y" {
		b.tmux.SendKeys(windowID, "y")
		b.tmux.SendEnter(windowID)
	} else if action == "n" {
		b.tmux.SendKeys(windowID, "n")
		b.tmux.SendEnter(windowID)
	} else if action != "refresh" {
		if keyName, ok := specialKeyMap[action]; ok {
			b.tmux.SendSpecialKey(windowID, keyName)
		}
	}

	time.Sleep(300 * time.Millisecond)
	b.sendScreenshotToChat(ctx, chatID, threadID, windowID)
}

// handleNavAction 处理交互式导航键盘按钮
func (b *Bot) handleNavAction(ctx context.Context, chatID int64, threadID int, action string, windowID string) {
	if action != "refresh" {
		if keyName, ok := specialKeyMap[action]; ok {
			b.tmux.SendSpecialKey(windowID, keyName)
		}
	}

	time.Sleep(300 * time.Millisecond)
	b.sendScreenshotToChat(ctx, chatID, threadID, windowID)
}

// sendMsg 发送消息到指定 chat/thread
func (b *Bot) sendMsg(ctx context.Context, chatID int64, threadID int, text string, kb *models.InlineKeyboardMarkup) {
	params := &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}
	if threadID != 0 {
		params.MessageThreadID = threadID
	}
	if kb != nil {
		params.ReplyMarkup = *kb
	}
	b.bot.SendMessage(ctx, params)
}

// expandHome 展开 ~ 路径
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// listSubDirs 列出目录下的子目录
func listSubDirs(path string) ([]DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var dirs []DirEntry
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, DirEntry{Name: e.Name(), IsDir: true})
		}
	}
	return dirs, nil
}
