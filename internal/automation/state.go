package automation

import (
	"encoding/json"
	"os"
	"sync"
)

// StateStore persists per-automation enabled/disabled overrides (keyed by automation
// name) to a JSON file, kept SEPARATE from config.yaml so UI toggles never rewrite the
// hand-commented config. The same store is shared across engine restarts — a config
// reload recreates the engine, but the enabled/disabled state must survive.
type StateStore struct {
	mu       sync.RWMutex
	path     string
	disabled map[string]bool // automation name -> true if the user disabled it
}

// NewStateStore loads any existing overrides from path (missing file = all enabled).
func NewStateStore(path string) *StateStore {
	s := &StateStore{path: path, disabled: map[string]bool{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.disabled)
	}
	return s
}

// Enabled reports whether an automation should run (default true unless disabled).
func (s *StateStore) Enabled(name string) bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.disabled[name]
}

// SetEnabled flips an automation on/off and persists the change immediately.
func (s *StateStore) SetEnabled(name string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		delete(s.disabled, name)
	} else {
		s.disabled[name] = true
	}
	b, err := json.MarshalIndent(s.disabled, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}
