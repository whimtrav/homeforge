package entity

import (
	"encoding/json"
	"log/slog"
	"os"
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
	mu           sync.RWMutex
	entities     map[string]*Entity
	restore      map[string]string // state loaded from snapshot at startup
	snapshotPath string
	saveCh       chan struct{}
	bus          *bus.Bus
}

func NewStore(b *bus.Bus, snapshotPath string) *Store {
	s := &Store{
		entities:     make(map[string]*Entity),
		restore:      make(map[string]string),
		snapshotPath: snapshotPath,
		saveCh:       make(chan struct{}, 1),
		bus:          b,
	}
	if snapshotPath != "" {
		s.loadSnapshot()
		go s.snapshotWriter()
	}
	return s
}

func (s *Store) loadSnapshot() {
	data, err := os.ReadFile(s.snapshotPath)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &s.restore); err != nil {
		slog.Warn("entity: snapshot parse failed", "err", err)
	}
	slog.Info("entity: snapshot loaded", "count", len(s.restore))
}

func (s *Store) snapshotWriter() {
	for range s.saveCh {
		s.writeSnapshot()
	}
}

func (s *Store) writeSnapshot() {
	s.mu.RLock()
	snap := make(map[string]string, len(s.entities))
	for id, e := range s.entities {
		snap[id] = e.State
	}
	s.mu.RUnlock()
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(s.snapshotPath, data, 0644); err != nil {
		slog.Warn("entity: snapshot write failed", "err", err)
	}
}

// Restored returns the state saved in the boot snapshot for id, if present. Accumulators
// (e.g. the water total) must seed from this BEFORE the entity is first Set() this session —
// Get() only sees materialized entities, so seeding from Get() alone starts the accumulator
// at 0 and then clobbers the restored value on the next write.
func (s *Store) Restored(id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.restore[id]
	return v, ok
}

func (s *Store) triggerSave() {
	select {
	case s.saveCh <- struct{}{}:
	default:
	}
}

func (s *Store) Set(e Entity) {
	s.mu.Lock()
	old := ""
	existed := false
	if existing, ok := s.entities[e.ID]; ok {
		old = existing.State
		existed = true
	}
	now := time.Now()
	// For new entities: restore saved state if available (survives restarts)
	if !existed {
		if saved, ok := s.restore[e.ID]; ok {
			e.State = saved
		}
	}
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

	if existed && old != e.State {
		s.triggerSave()
		s.bus.Publish(TopicStateChanged, StateChangedPayload{
			Entity:   e,
			OldState: old,
		})
	} else if !existed {
		s.triggerSave()
	} else if e.Domain == "climate" {
		// Climate entities keep their live data (setpoint, current_temperature,
		// hvac_action, preset_mode) in ATTRIBUTES — their STATE is only the mode. So
		// push on every update even when the mode is unchanged, or the UI's thermostat
		// card never sees a setpoint/temp change and appears frozen. Low volume (one
		// entity, ~30s tick + on control changes), so no WS flooding.
		s.bus.Publish(TopicStateChanged, StateChangedPayload{
			Entity:   e,
			OldState: old,
		})
	}
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
