package entity

import (
	"sync"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
)

// Entity is the core data model — mirrors an HA entity.
type Entity struct {
	ID          string         `json:"id"`           // "light.hallway"
	Name        string         `json:"name"`         // "Hallway Light"
	Domain      string         `json:"domain"`       // "light"
	State       string         `json:"state"`        // "on" / "off" / numeric string
	Attributes  map[string]any `json:"attributes"`
	LastChanged time.Time      `json:"last_changed"`
	LastUpdated time.Time      `json:"last_updated"`
}

const TopicStateChanged = "entity.state_changed"

type StateChangedPayload struct {
	Entity   Entity
	OldState string
}

type Store struct {
	mu       sync.RWMutex
	entities map[string]*Entity
	bus      *bus.Bus
}

func NewStore(b *bus.Bus) *Store {
	return &Store{
		entities: make(map[string]*Entity),
		bus:      b,
	}
}

func (s *Store) Set(e Entity) {
	s.mu.Lock()
	old := ""
	if existing, ok := s.entities[e.ID]; ok {
		old = existing.State
	}
	now := time.Now()
	if old != e.State {
		e.LastChanged = now
	} else if existing, ok := s.entities[e.ID]; ok {
		e.LastChanged = existing.LastChanged
	}
	e.LastUpdated = now
	if e.Attributes == nil {
		e.Attributes = make(map[string]any)
	}
	s.entities[e.ID] = &e
	s.mu.Unlock()

	s.bus.Publish(TopicStateChanged, StateChangedPayload{
		Entity:   e,
		OldState: old,
	})
}

func (s *Store) Get(id string) (Entity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entities[id]
	if !ok {
		return Entity{}, false
	}
	return *e, true
}

func (s *Store) All() []Entity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entity, 0, len(s.entities))
	for _, e := range s.entities {
		out = append(out, *e)
	}
	return out
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entities, id)
}
