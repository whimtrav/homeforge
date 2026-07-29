package api

// Durable memory for the assistant: facts the user asks it to remember, persisted to a small
// JSON file in /data so they survive restarts and appear in every future chat. Saved facts are
// injected into the (static) system prompt — the "knowledge lives outside the model's weights,
// retrieved per turn" pattern, so a small frozen model effectively knows whatever it's told.
// A JSON file is the fit-for-purpose store for a handful of notes; pgvector/TimescaleDB is the
// upgrade path if we ever want semantic recall over many facts.

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memFact struct {
	ID    int    `json:"id"`
	Text  string `json:"text"`
	Added string `json:"added"`
}

type assistantMemory struct {
	mu     sync.Mutex
	path   string
	facts  []memFact
	nextID int
}

func newAssistantMemory(path string) *assistantMemory {
	m := &assistantMemory{path: path, nextID: 1}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m.facts)
		for _, f := range m.facts {
			if f.ID >= m.nextID {
				m.nextID = f.ID + 1
			}
		}
	}
	return m
}

// persist writes atomically; caller must hold m.mu.
func (m *assistantMemory) persist() {
	data, _ := json.MarshalIndent(m.facts, "", "  ")
	tmp := m.path + ".tmp"
	if os.WriteFile(tmp, data, 0644) == nil {
		os.Rename(tmp, m.path)
	}
}

func (m *assistantMemory) add(text string) memFact {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := memFact{ID: m.nextID, Text: strings.TrimSpace(text), Added: time.Now().Format("2006-01-02")}
	m.nextID++
	m.facts = append(m.facts, f)
	m.persist()
	return f
}

// forget removes facts matching an exact id or a keyword substring; returns what was removed.
func (m *assistantMemory) forget(query string) []memFact {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	wantID, isID := strconv.Atoi(q)
	var removed, kept []memFact
	for _, f := range m.facts {
		match := (isID == nil && f.ID == wantID) || strings.Contains(strings.ToLower(f.Text), q)
		if match {
			removed = append(removed, f)
		} else {
			kept = append(kept, f)
		}
	}
	if len(removed) > 0 {
		m.facts = kept
		m.persist()
	}
	return removed
}

func (m *assistantMemory) all() []memFact {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]memFact, len(m.facts))
	copy(out, m.facts)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
