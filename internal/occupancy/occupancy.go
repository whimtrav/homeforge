// Package occupancy computes ONE "is anyone home?" signal — binary_sensor.home_occupied — from
// every available presence source, so the thermostat's auto-away can never fire while the house
// is in use. Sources are auto-discovered by pattern:
//   - device_tracker.* == "home"                         → a phone is home
//   - switch.*_presence == "on"                          → mmwave presence (sustained)
//   - motion binary_sensors / switch.*_motion == "on"    → motion (sustained while active)
//   - number.*_button_c increments                       → a physical switch was pressed (a real
//     finger, NOT an automated command)
//
// Occupied if any source is present now, OR there was motion / a physical press within the grace
// window. Reads "empty" only once all phones are away AND no motion/press for grace_min minutes.
// Fails safe toward occupied (starts occupied; any new source keeps it occupied).
package occupancy

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

const occupiedEntityID = "binary_sensor.home_occupied"

type Manager struct {
	cfg   config.OccupancyConfig
	store *entity.Store
	bus   *bus.Bus
	grace time.Duration

	mu           sync.Mutex
	trackerHome  map[string]bool    // device_tracker.* → home?
	sensorOn     map[string]bool    // motion + mmwave → on?
	pressCounts  map[string]float64 // number.*_button_c → last value
	lastActivity time.Time
	lastState    string
}

func NewManager(cfg config.OccupancyConfig, store *entity.Store, b *bus.Bus) *Manager {
	g := cfg.GraceMin
	if g <= 0 {
		g = 20
	}
	return &Manager{
		cfg: cfg, store: store, bus: b, grace: time.Duration(g) * time.Minute,
		trackerHome: map[string]bool{}, sensorOn: map[string]bool{}, pressCounts: map[string]float64{},
	}
}

func isTracker(id string) bool { return strings.HasPrefix(id, "device_tracker.") }

func isMmwave(id string) bool {
	return strings.HasPrefix(id, "switch.") && strings.HasSuffix(id, "_presence")
}

func isMotion(e entity.Entity) bool {
	if strings.Contains(e.ID, "tamper") {
		return false
	}
	if strings.HasPrefix(e.ID, "binary_sensor.") {
		if dc, _ := e.Attributes["device_class"].(string); dc == "motion" {
			return true
		}
	}
	return strings.HasSuffix(e.ID, "_motion") // LiquidFW motion pins arrive as switch.<dev>_motion
}

func isButton(id string) bool {
	return strings.HasPrefix(id, "number.") && strings.HasSuffix(id, "_button_c")
}

func (m *Manager) Run(ctx context.Context) {
	m.seed()
	m.mu.Lock()
	m.lastActivity = time.Now() // assume occupied at startup (fail safe)
	m.mu.Unlock()

	m.bus.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
		if p, ok := ev.Payload.(entity.StateChangedPayload); ok {
			m.onChange(p.Entity)
		}
	})
	m.publish()
	slog.Info("occupancy: started", "grace_min", int(m.grace.Minutes()))

	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.publish() // re-evaluate grace expiry
		}
	}
}

// seed current states so existing values aren't mistaken for fresh activity at startup.
func (m *Manager) seed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.store.All() {
		switch {
		case isTracker(e.ID):
			m.trackerHome[e.ID] = strings.EqualFold(e.State, "home")
		case isMmwave(e.ID) || isMotion(e):
			m.sensorOn[e.ID] = strings.EqualFold(e.State, "on")
		case isButton(e.ID):
			if v, err := strconv.ParseFloat(e.State, 64); err == nil {
				m.pressCounts[e.ID] = v
			}
		}
	}
}

func (m *Manager) onChange(e entity.Entity) {
	id := e.ID
	relevant := false
	m.mu.Lock()
	switch {
	case isTracker(id):
		m.trackerHome[id] = strings.EqualFold(e.State, "home")
		relevant = true
	case isMmwave(id) || isMotion(e):
		on := strings.EqualFold(e.State, "on")
		if on && !m.sensorOn[id] {
			m.lastActivity = time.Now() // fresh motion
		}
		m.sensorOn[id] = on
		relevant = true
	case isButton(id):
		if v, err := strconv.ParseFloat(e.State, 64); err == nil {
			if prev, ok := m.pressCounts[id]; ok && v != prev {
				m.lastActivity = time.Now() // physical press
			}
			m.pressCounts[id] = v
		}
		relevant = true
	}
	m.mu.Unlock()
	if relevant {
		m.publish()
	}
}

func (m *Manager) presentNow() bool {
	for _, h := range m.trackerHome {
		if h {
			return true
		}
	}
	for _, on := range m.sensorOn {
		if on {
			return true
		}
	}
	return false
}

func (m *Manager) publish() {
	m.mu.Lock()
	present := m.presentNow()
	occ := present || time.Since(m.lastActivity) < m.grace
	reason := "empty"
	if present {
		reason = "present"
	} else if occ {
		reason = "recent activity"
	}
	last := m.lastActivity
	state := "off"
	if occ {
		state = "on"
	}
	changed := state != m.lastState
	m.lastState = state
	m.mu.Unlock()

	m.store.Set(entity.Entity{
		ID: occupiedEntityID, Name: "Home Occupied", Domain: "binary_sensor", State: state,
		Attributes: map[string]any{
			"device_class": "occupancy", "friendly_name": "Home Occupied", "section": "presence",
			"source": "occupancy", "reason": reason, "last_activity": last.Format(time.RFC3339),
		},
	})
	if changed {
		slog.Info("occupancy", "state", state, "reason", reason)
	}
}
