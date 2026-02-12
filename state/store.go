package state

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Binding struct {
	WindowID    string    `json:"window_id"`
	Backend     string    `json:"backend"`
	ProjectPath string    `json:"project_path"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	Status      string    `json:"status"` // "running" | "disconnected"
}

type Offset struct {
	File         string `json:"file"`
	ByteOffset   int64  `json:"byte_offset"`
	MessageCount int    `json:"message_count"` // Gemini 专用
}

type DirState struct {
	Favorites []string `json:"favorites"`
	Recent    []string `json:"recent"`
}

type stateData struct {
	Bindings map[string]Binding `json:"bindings"`
	Offsets  map[string]Offset  `json:"offsets"`
	Dirs     DirState           `json:"dirs"`
}

type Store struct {
	mu        sync.RWMutex
	data      stateData
	path      string
	recentMax int
	saveCh    chan struct{}
	done      chan struct{}
}

func New(path string, recentMax int) *Store {
	s := &Store{
		path:      path,
		recentMax: recentMax,
		saveCh:    make(chan struct{}, 1),
		done:      make(chan struct{}),
		data: stateData{
			Bindings: make(map[string]Binding),
			Offsets:  make(map[string]Offset),
		},
	}

	// 尝试加载已有文件
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &s.data); err != nil {
			slog.Warn("failed to parse state file, starting fresh", "error", err)
			s.data.Bindings = make(map[string]Binding)
			s.data.Offsets = make(map[string]Offset)
		}
	}
	if s.data.Bindings == nil {
		s.data.Bindings = make(map[string]Binding)
	}
	if s.data.Offsets == nil {
		s.data.Offsets = make(map[string]Offset)
	}

	// 启动异步刷盘 goroutine
	go s.asyncSaveLoop()
	return s
}

// asyncSaveLoop debounce 500ms 异步刷盘
func (s *Store) asyncSaveLoop() {
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	for {
		select {
		case <-s.saveCh:
			timer.Reset(500 * time.Millisecond)
		case <-timer.C:
			if err := s.Save(); err != nil {
				slog.Error("failed to save state", "error", err)
			}
		case <-s.done:
			timer.Stop()
			return
		}
	}
}

func (s *Store) triggerSave() {
	select {
	case s.saveCh <- struct{}{}:
	default:
	}
}

// Binding 操作
func (s *Store) SetBinding(topicKey string, b Binding) {
	s.mu.Lock()
	s.data.Bindings[topicKey] = b
	s.mu.Unlock()
	s.triggerSave()
}

func (s *Store) GetBinding(topicKey string) (Binding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.data.Bindings[topicKey]
	return b, ok
}

func (s *Store) DeleteBinding(topicKey string) {
	s.mu.Lock()
	delete(s.data.Bindings, topicKey)
	s.mu.Unlock()
	s.triggerSave()
}

func (s *Store) AllBindings() map[string]Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]Binding, len(s.data.Bindings))
	for k, v := range s.data.Bindings {
		result[k] = v
	}
	return result
}

// Offset 操作
func (s *Store) SetOffset(topicKey string, o Offset) {
	s.mu.Lock()
	s.data.Offsets[topicKey] = o
	s.mu.Unlock()
	s.triggerSave()
}

func (s *Store) GetOffset(topicKey string) (Offset, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.data.Offsets[topicKey]
	return o, ok
}

func (s *Store) DeleteOffset(topicKey string) {
	s.mu.Lock()
	delete(s.data.Offsets, topicKey)
	s.mu.Unlock()
	s.triggerSave()
}

// Dir 操作
func (s *Store) AddFavorite(path string) {
	s.mu.Lock()
	for _, f := range s.data.Dirs.Favorites {
		if f == path {
			s.mu.Unlock()
			return
		}
	}
	s.data.Dirs.Favorites = append(s.data.Dirs.Favorites, path)
	s.mu.Unlock()
	s.triggerSave()
}

func (s *Store) RemoveFavorite(path string) {
	s.mu.Lock()
	filtered := s.data.Dirs.Favorites[:0]
	for _, f := range s.data.Dirs.Favorites {
		if f != path {
			filtered = append(filtered, f)
		}
	}
	s.data.Dirs.Favorites = filtered
	s.mu.Unlock()
	s.triggerSave()
}

func (s *Store) AddRecent(path string) {
	s.mu.Lock()
	// 去重：先移除已有
	filtered := make([]string, 0, len(s.data.Dirs.Recent))
	for _, r := range s.data.Dirs.Recent {
		if r != path {
			filtered = append(filtered, r)
		}
	}
	// 头部插入
	s.data.Dirs.Recent = append([]string{path}, filtered...)
	// 截断到 recentMax
	if len(s.data.Dirs.Recent) > s.recentMax {
		s.data.Dirs.Recent = s.data.Dirs.Recent[:s.recentMax]
	}
	s.mu.Unlock()
	s.triggerSave()
}

func (s *Store) GetDirs() DirState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return DirState{
		Favorites: append([]string{}, s.data.Dirs.Favorites...),
		Recent:    append([]string{}, s.data.Dirs.Recent...),
	}
}

// Save 同步保存到文件
func (s *Store) Save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	// 确保目录存在
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// Close 最终刷盘并停止 goroutine
func (s *Store) Close() {
	close(s.done)
	if err := s.Save(); err != nil {
		slog.Error("failed to save state on close", "error", err)
	}
}
