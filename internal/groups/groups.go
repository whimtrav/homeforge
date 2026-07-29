// Package groups provides virtual switch entities that fan a service call out to a
// set of member entities and reflect their combined state. Used for room light
// groups (e.g. the two upbath WiZ bulbs) so automations and the UI have one target.
package groups

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

type Manager struct {
	groups   map[string]config.GroupConfig // group entity id -> config
	memberOf map[string][]string           // member entity id -> group entity ids
	store    *entity.Store
	bus      *bus.Bus
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "_"), "_")
}

func NewManager(groups []config.GroupConfig, store *entity.Store, b *bus.Bus) *Manager {
	m := &Manager{
		groups:   map[string]config.GroupConfig{},
		memberOf: map[string][]string{},
		store:    store,
		bus:      b,
	}
	for _, g := range groups {
		if g.Name == "" || len(g.Members) == 0 {
			continue
		}
		id := "switch." + slug(g.Name)
		m.groups[id] = g
		for _, mem := range g.Members {
			m.memberOf[mem] = append(m.memberOf[mem], id)
		}
	}
	return m
}

func (m *Manager) Run(ctx context.Context) {
	if len(m.groups) == 0 {
		return
	}

	// Register each group as a switch entity, seeded from its members' current state.
	for id, g := range m.groups {
		m.publish(id, g)
	}
	slog.Info("groups: registered", "count", len(m.groups))

	// Fan a service call targeting a group out to its members.
	m.bus.Subscribe("service.call", func(ev bus.Event) {
		p, ok := ev.Payload.(map[string]any)
		if !ok {
			return
		}
		entityID, _ := p["entity"].(string)
		g, isGroup := m.groups[entityID]
		if !isGroup {
			return
		}
		service, _ := p["service"].(string)
		data, _ := p["data"].(map[string]any)
		// Resolve toggle to an explicit turn_on/turn_off from the group's state so
		// all members end in the SAME state (not each flipped individually).
		if strings.HasSuffix(strings.ToLower(service), ".toggle") {
			if m.state(g) == "on" {
				service = "switch.turn_off"
			} else {
				service = "switch.turn_on"
			}
		}
		for _, mem := range g.Members {
			m.bus.Publish("service.call", map[string]any{
				"service": service,
				"entity":  mem,
				"data":    data,
			})
		}
	})

	// Recompute a group's state whenever one of its members changes.
	m.bus.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
		payload, ok := ev.Payload.(entity.StateChangedPayload)
		if !ok {
			return
		}
		for _, gid := range m.memberOf[payload.Entity.ID] {
			m.publish(gid, m.groups[gid])
		}
	})

	// Periodic reconcile: members can be created AFTER this manager registered
	// (startup race) or re-set without a state change, so the state_changed hook
	// alone can leave a group stale. publish() is a no-op when unchanged.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for id, g := range m.groups {
				m.publish(id, g)
			}
		}
	}
}

func (m *Manager) publish(id string, g config.GroupConfig) {
	m.store.Set(entity.Entity{
		ID:         id,
		Name:       g.Name,
		Domain:     "switch",
		State:      m.state(g),
		Attributes: map[string]any{"group": true, "members": g.Members},
	})
}

// state = "on" if any member is on, else "off".
func (m *Manager) state(g config.GroupConfig) string {
	for _, mem := range g.Members {
		if e, ok := m.store.Get(mem); ok && (e.State == "on" || e.State == "ON") {
			return "on"
		}
	}
	return "off"
}
