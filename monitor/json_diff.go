package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/user/tgmux/state"
)

// GeminiLogEntry logs.json 中的条目
type GeminiLogEntry struct {
	SessionID string `json:"sessionId"`
	MessageID int    `json:"messageId"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// JSONDiffMonitor Gemini JSON 全量 diff 监控
type JSONDiffMonitor struct {
	topicKey      string
	tmpDir        string
	lastMessageID int
	startTime     time.Time
	handler       OutputHandler
	store         *state.Store
	cancel        context.CancelFunc
	mu            sync.Mutex
	lockedHashDir string
}

func NewJSONDiffMonitor(topicKey, tmpDir string, lastMessageID int, startTime time.Time, handler OutputHandler, store *state.Store) *JSONDiffMonitor {
	return &JSONDiffMonitor{
		topicKey:      topicKey,
		tmpDir:        tmpDir,
		lastMessageID: lastMessageID,
		startTime:     startTime,
		handler:       handler,
		store:         store,
	}
}

func (m *JSONDiffMonitor) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}

	if _, err := os.Stat(m.tmpDir); os.IsNotExist(err) {
		watcher.Close()
		return fmt.Errorf("gemini tmp dir not found: %s", m.tmpDir)
	}

	// 先扫描已有目录
	m.lockedHashDir = m.scanExistingDirs()

	if m.lockedHashDir != "" {
		if err := watcher.Add(m.lockedHashDir); err != nil {
			watcher.Close()
			return fmt.Errorf("watch hash dir: %w", err)
		}
	} else {
		if err := watcher.Add(m.tmpDir); err != nil {
			watcher.Close()
			return fmt.Errorf("watch tmp dir: %w", err)
		}
	}

	ctx, m.cancel = context.WithCancel(ctx)
	go m.loop(ctx, watcher)
	return nil
}

func (m *JSONDiffMonitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *JSONDiffMonitor) loop(ctx context.Context, watcher *fsnotify.Watcher) {
	defer watcher.Close()

	timeout := time.NewTimer(30 * time.Second)
	if m.lockedHashDir != "" {
		timeout.Stop()
	}
	defer timeout.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			m.handleEvent(watcher, event, timeout)
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("gemini watcher error", "key", m.topicKey, "error", err)
		case <-timeout.C:
			slog.Warn("gemini hash dir detection timeout", "key", m.topicKey)
			m.handler(m.topicKey, "无法定位 Gemini 日志目录，已切换为终端捕获模式", ContentText)
			return
		}
	}
}

func (m *JSONDiffMonitor) handleEvent(watcher *fsnotify.Watcher, event fsnotify.Event, timeout *time.Timer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lockedHashDir == "" {
		if event.Has(fsnotify.Create) {
			info, err := os.Stat(event.Name)
			if err != nil || !info.IsDir() {
				return
			}
			if info.ModTime().After(m.startTime.Add(-2 * time.Second)) {
				m.lockedHashDir = event.Name
				slog.Info("locked gemini hash dir", "key", m.topicKey, "dir", event.Name)
				watcher.Remove(m.tmpDir)
				watcher.Add(m.lockedHashDir)
				timeout.Stop()
				m.readAndDiff()
			}
		}
		return
	}

	if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
		name := filepath.Base(event.Name)
		if name == "logs.json" {
			m.readAndDiff()
		}
	}
}

func (m *JSONDiffMonitor) readAndDiff() {
	logsPath := filepath.Join(m.lockedHashDir, "logs.json")
	data, err := os.ReadFile(logsPath)
	if err != nil {
		return
	}

	var entries []GeminiLogEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Debug("gemini json parse failed, skipping", "key", m.topicKey, "error", err)
		return
	}

	var newTexts []string
	for _, entry := range entries {
		if entry.MessageID > m.lastMessageID && entry.Type == "model" {
			if entry.Message != "" {
				newTexts = append(newTexts, entry.Message)
			}
			m.lastMessageID = entry.MessageID
		}
	}

	if len(newTexts) > 0 {
		m.store.SetOffset(m.topicKey, state.Offset{
			File:         filepath.Join(m.lockedHashDir, "logs.json"),
			MessageCount: m.lastMessageID,
		})
		combined := strings.Join(newTexts, "\n")
		m.handler(m.topicKey, combined, ContentText)
	}
}

func (m *JSONDiffMonitor) scanExistingDirs() string {
	entries, err := os.ReadDir(m.tmpDir)
	if err != nil {
		return ""
	}

	var bestDir string
	var bestTime time.Time
	threshold := m.startTime.Add(-2 * time.Second)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(threshold) && info.ModTime().After(bestTime) {
			bestDir = filepath.Join(m.tmpDir, e.Name())
			bestTime = info.ModTime()
		}
	}
	return bestDir
}
