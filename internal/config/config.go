package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Hotkey describes a parsed hotkey chord, e.g. Alt+Q.
type Hotkey struct {
	ModAlt   bool
	ModCtrl  bool
	ModShift bool
	ModWin   bool
	Key      byte // virtual-key code for letters/digits
}

// Config mirrors config.json next to the executable.
type Config struct {
	GUIProcesses      []string          `json:"gui_processes"`
	TerminalProcesses []string          `json:"terminal_processes"`
	Hotkeys           HotkeyConfig      `json:"hotkeys"`
	ClipboardTimeout  int               `json:"clipboard_timeout_ms"`
	RestoreDelay      int               `json:"restore_clipboard_delay_ms"`
	SlangDictionary   map[string]string `json:"slang_dictionary"`
}

type HotkeyConfig struct {
	InPlace string `json:"inplace"`
	Popup   string `json:"popup"`
}

// Store loads config.json once and transparently reloads it when the
// file's mtime changes, so edits apply without restarting the daemon.
type Store struct {
	path    string
	mu      sync.RWMutex
	cfg     *Config
	modTime time.Time
}

func Load(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Locate finds config.json next to the executable, falling back to the
// working directory (useful for `go run`).
func Locate() string {
	exe, err := os.Executable()
	if err == nil {
		p := filepath.Join(filepath.Dir(exe), "config.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "config.json"
}

// Get returns the current config, reloading it if the file changed.
func (s *Store) Get() *Config {
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) maybeReload() {
	fi, err := os.Stat(s.path)
	if err != nil {
		return
	}
	s.mu.RLock()
	fresh := fi.ModTime().After(s.modTime)
	s.mu.RUnlock()
	if fresh {
		_ = s.Reload()
	}
}

// Reload re-reads the file unconditionally.
func (s *Store) Reload() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	cfg.setDefaults()

	fi, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = &cfg
	s.modTime = fi.ModTime()
	s.mu.Unlock()
	return nil
}

func (c *Config) setDefaults() {
	if c.ClipboardTimeout <= 0 {
		c.ClipboardTimeout = 300
	}
	if c.RestoreDelay <= 0 {
		c.RestoreDelay = 150
	}
	if c.Hotkeys.InPlace == "" {
		c.Hotkeys.InPlace = "alt+q"
	}
	if c.Hotkeys.Popup == "" {
		c.Hotkeys.Popup = "alt+w"
	}
	if c.SlangDictionary == nil {
		c.SlangDictionary = map[string]string{}
	}
}

// ProcessClass categorizes a foreground executable name.
type ProcessClass int

const (
	ClassIgnored ProcessClass = iota
	ClassGUI
	ClassTerminal
)

func (c *Config) Classify(exeName string) ProcessClass {
	name := strings.ToLower(filepath.Base(exeName))
	for _, p := range c.GUIProcesses {
		if strings.EqualFold(p, name) {
			return ClassGUI
		}
	}
	for _, p := range c.TerminalProcesses {
		if strings.EqualFold(p, name) {
			return ClassTerminal
		}
	}
	return ClassIgnored
}

// ParseHotkey converts "alt+q" / "ctrl+shift+f5" into modifiers + VK code.
// Only the VK codes actually needed (A-Z, 0-9, F1-F24) are resolved here.
func ParseHotkey(s string) (Hotkey, error) {
	var h Hotkey
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "+")
	if len(parts) == 0 {
		return h, fmt.Errorf("empty hotkey")
	}
	key := parts[len(parts)-1]
	for _, mod := range parts[:len(parts)-1] {
		switch strings.TrimSpace(mod) {
		case "alt":
			h.ModAlt = true
		case "ctrl", "control":
			h.ModCtrl = true
		case "shift":
			h.ModShift = true
		case "win":
			h.ModWin = true
		default:
			return h, fmt.Errorf("unknown modifier %q", mod)
		}
	}
	switch {
	case len(key) == 1 && key[0] >= 'a' && key[0] <= 'z':
		h.Key = 'A' + (key[0] - 'a')
	case len(key) == 1 && key[0] >= '0' && key[0] <= '9':
		h.Key = key[0]
	default:
		return h, fmt.Errorf("unsupported key %q (letters and digits only)", key)
	}
	return h, nil
}
