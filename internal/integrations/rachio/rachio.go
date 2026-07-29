// Package rachio is a self-contained HomeForge integration for Rachio smart
// irrigation controllers, replacing HA's cloud integration.
//
// Rachio has a clean public REST API (https://api.rach.io/1/public/) with a
// bearer api_key — the same key HA uses. This integration resolves the account's
// device(s) + zones once, then polls status and exposes:
//   - switch.rachio_<zone>            (on = that zone is currently watering)
//   - binary_sensor.rachio_<dev>_online
//   - binary_sensor.rachio_<dev>_rain (rain sensor tripped)
// and accepts homeforge service calls: turn_on a zone → start it (default 5 min,
// or data.duration seconds); turn_off → stop all watering on the device.
// Lives entirely in the entity store — no MQTT/HA.
package rachio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

const apiBase = "https://api.rach.io/1/public/"

var rachioHTTP = &http.Client{Timeout: 10 * time.Second}

type zone struct {
	id   string
	name string
	num  int
}

type device struct {
	id    string
	name  string
	zones []zone
}

// Manager is the Rachio integration entry point.
type Manager struct {
	cfg   config.RachioConfig
	store *entity.Store
	bus   *bus.Bus

	mu      sync.Mutex
	devices []device
}

func NewManager(cfg config.RachioConfig, store *entity.Store, b *bus.Bus) *Manager {
	return &Manager{cfg: cfg, store: store, bus: b}
}

func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("rachio: disabled")
		<-ctx.Done()
		return
	}
	if m.cfg.APIKey == "" {
		slog.Warn("rachio: enabled but no api_key — disabling")
		<-ctx.Done()
		return
	}
	poll := time.Duration(m.cfg.PollSeconds) * time.Second
	if poll <= 0 {
		poll = 30 * time.Second
	}

	// Resolve devices + zones (retry until the cloud answers).
	for ctx.Err() == nil {
		if err := m.discover(); err != nil {
			slog.Warn("rachio: discover failed, retrying", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
				continue
			}
		}
		break
	}
	m.mu.Lock()
	nz := 0
	for _, d := range m.devices {
		nz += len(d.zones)
	}
	nd := len(m.devices)
	m.mu.Unlock()
	slog.Info("rachio: starting", "devices", nd, "zones", nz)

	m.bus.Subscribe("service.call", func(ev bus.Event) {
		p, ok := ev.Payload.(map[string]any)
		if !ok {
			return
		}
		entityID, _ := p["entity"].(string)
		service, _ := p["service"].(string)
		data, _ := p["data"].(map[string]any)
		m.handleServiceCall(entityID, service, data)
	})

	m.pollAll()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.pollAll()
		}
	}
}

// ---- REST ----

func (m *Manager) api(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, apiBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := rachioHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rachio: %s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (m *Manager) discover() error {
	var pinfo struct {
		ID string `json:"id"`
	}
	b, err := m.api("GET", "person/info", nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &pinfo); err != nil || pinfo.ID == "" {
		return fmt.Errorf("rachio: no person id")
	}
	var person struct {
		Devices []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Zones []struct {
				ID         string `json:"id"`
				Name       string `json:"name"`
				ZoneNumber int    `json:"zoneNumber"`
				Enabled    bool   `json:"enabled"`
			} `json:"zones"`
		} `json:"devices"`
	}
	b, err = m.api("GET", "person/"+pinfo.ID, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &person); err != nil {
		return err
	}
	var devs []device
	for _, d := range person.Devices {
		dv := device{id: d.ID, name: d.Name}
		for _, z := range d.Zones {
			if z.Enabled {
				dv.zones = append(dv.zones, zone{id: z.ID, name: z.Name, num: z.ZoneNumber})
			}
		}
		devs = append(devs, dv)
	}
	m.mu.Lock()
	m.devices = devs
	m.mu.Unlock()
	return nil
}

// ---- polling ----

func (m *Manager) pollAll() {
	m.mu.Lock()
	devs := append([]device(nil), m.devices...)
	m.mu.Unlock()
	for _, d := range devs {
		m.pollOne(d)
	}
}

func (m *Manager) pollOne(d device) {
	// device status: online + rain sensor
	var ds struct {
		Status            string `json:"status"`
		On                bool   `json:"on"`
		RainSensorTripped bool   `json:"rainSensorTripped"`
	}
	if b, err := m.api("GET", "device/"+d.id, nil); err == nil {
		_ = json.Unmarshal(b, &ds)
	} else {
		return // offline; keep last state
	}

	// currently-running zone (empty {} when idle)
	runningZone := ""
	if b, err := m.api("GET", "device/"+d.id+"/current_schedule", nil); err == nil {
		var cs struct {
			ZoneID string `json:"zoneId"`
		}
		_ = json.Unmarshal(b, &cs)
		runningZone = cs.ZoneID
	}

	dslug := slugify(d.name)
	online := "off"
	if strings.EqualFold(ds.Status, "ONLINE") {
		online = "on"
	}
	m.store.Set(entity.Entity{
		ID: "binary_sensor.rachio_" + dslug + "_online", Name: d.name + " Online", Domain: "binary_sensor",
		State: online, Attributes: map[string]any{"device": d.name, "rachio_device_id": d.id},
	})
	rain := "off"
	if ds.RainSensorTripped {
		rain = "on"
	}
	m.store.Set(entity.Entity{
		ID: "binary_sensor.rachio_" + dslug + "_rain", Name: d.name + " Rain Sensor", Domain: "binary_sensor",
		State: rain, Attributes: map[string]any{"device": d.name, "rachio_device_id": d.id},
	})

	for _, z := range d.zones {
		state := "off"
		if z.id == runningZone {
			state = "on"
		}
		m.store.Set(entity.Entity{
			ID: "switch.rachio_" + slugify(z.name), Name: z.name, Domain: "switch", State: state,
			Attributes: map[string]any{
				"device":           d.name,
				"rachio_kind":      "zone",
				"rachio_zone_id":   z.id,
				"rachio_device_id": d.id,
				"zone_number":      z.num,
			},
		})
	}
}

// ---- commands ----

func (m *Manager) handleServiceCall(entityID, service string, data map[string]any) {
	if entityID == "" {
		return
	}
	e, ok := m.store.Get(entityID)
	if !ok {
		return
	}
	if kind, _ := e.Attributes["rachio_kind"].(string); kind != "zone" {
		return // not a rachio zone entity
	}
	zoneID, _ := e.Attributes["rachio_zone_id"].(string)
	deviceID, _ := e.Attributes["rachio_device_id"].(string)
	lc := strings.ToLower(service)

	switch {
	case strings.HasSuffix(lc, ".turn_on"):
		dur := 300
		if v, ok := numAny(data["duration"]); ok && v > 0 {
			dur = int(v)
		}
		if dur > 10800 {
			dur = 10800
		}
		if _, err := m.api("PUT", "zone/start", map[string]any{"id": zoneID, "duration": dur}); err != nil {
			slog.Warn("rachio: zone start failed", "entity", entityID, "err", err)
			return
		}
		slog.Info("rachio: zone start", "entity", entityID, "duration", dur)
		e.State = "on"
		m.store.Set(e)
	case strings.HasSuffix(lc, ".turn_off"), strings.HasSuffix(lc, ".toggle"):
		if lc[strings.LastIndex(lc, ".")+1:] == "toggle" && e.State != "on" {
			// toggle while off → start
			m.handleServiceCall(entityID, strings.Replace(service, "toggle", "turn_on", 1), data)
			return
		}
		if _, err := m.api("PUT", "device/stop_water", map[string]any{"id": deviceID}); err != nil {
			slog.Warn("rachio: stop water failed", "entity", entityID, "err", err)
			return
		}
		slog.Info("rachio: stop water", "device", deviceID)
		// stop_water halts ALL zones on the device — reflect that optimistically
		m.mu.Lock()
		devs := append([]device(nil), m.devices...)
		m.mu.Unlock()
		for _, d := range devs {
			if d.id != deviceID {
				continue
			}
			for _, z := range d.zones {
				if ze, ok := m.store.Get("switch.rachio_" + slugify(z.name)); ok {
					ze.State = "off"
					m.store.Set(ze)
				}
			}
		}
	}
}

// ---- helpers ----

func slugify(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prev = false
		} else if !prev {
			b.WriteByte('_')
			prev = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func numAny(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
