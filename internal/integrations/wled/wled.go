// Package wled is a self-contained HomeForge integration for WLED controllers.
//
// WLED speaks HTTP JSON (GET /json/state, /json/info; POST /json/state). Unlike
// WiZ there's no broadcast discovery, so devices come from config (name + host).
// Mirrors the WiZ integration: poll state, publish switch+number entities, and
// accept homeforge service calls. Lives entirely in the entity store — no MQTT/HA.
package wled

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

const pollEvery = 5 * time.Second

var wledHTTP = &http.Client{Timeout: 4 * time.Second}

// dev is the cached view of one WLED controller.
type dev struct {
	name string
	host string
	// last-known primary colour, so a single-channel (r/g/b) slider change can be
	// re-sent as a full triple (WLED col wants [r,g,b]).
	r, g, b int
}

// Manager is the WLED integration entry point.
type Manager struct {
	cfg   config.WLEDConfig
	store *entity.Store
	bus   *bus.Bus

	mu   sync.Mutex
	devs map[string]*dev // keyed by config name
}

func NewManager(cfg config.WLEDConfig, store *entity.Store, b *bus.Bus) *Manager {
	devs := make(map[string]*dev)
	for _, d := range cfg.Devices {
		if d.Name != "" && d.Host != "" {
			devs[d.Name] = &dev{name: d.Name, host: d.Host}
		}
	}
	return &Manager{cfg: cfg, store: store, bus: b, devs: devs}
}

func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("wled: disabled")
		<-ctx.Done()
		return
	}
	slog.Info("wled: starting", "devices", len(m.devs))

	// Commands: shared service.call bus, act only on entities with a wled_host attr.
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
	t := time.NewTicker(pollEvery)
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

// ---- polling ----

func (m *Manager) pollAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.devs))
	for n := range m.devs {
		names = append(names, n)
	}
	m.mu.Unlock()
	for _, n := range names {
		m.pollOne(n)
	}
}

type wledState struct {
	On  bool `json:"on"`
	Bri int  `json:"bri"`
	Seg []struct {
		Col [][]int `json:"col"`
		Fx  int     `json:"fx"`
	} `json:"seg"`
}

type wledInfo struct {
	Name string `json:"name"`
	Wifi struct {
		Rssi int `json:"rssi"`
	} `json:"wifi"`
}

func (m *Manager) pollOne(name string) {
	m.mu.Lock()
	d := m.devs[name]
	m.mu.Unlock()
	if d == nil {
		return
	}
	var st wledState
	if err := getJSON("http://"+d.host+"/json/state", &st); err != nil {
		return // offline; keep last state
	}
	var info wledInfo
	_ = getJSON("http://"+d.host+"/json/info", &info)
	m.publish(d, st, info)
}

func getJSON(url string, out any) error {
	resp, err := wledHTTP.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// ---- publish ----

func (m *Manager) publish(d *dev, st wledState, info wledInfo) {
	slug := slugify(d.name)

	attrs := func(sub, unit string, min, max int) map[string]any {
		a := map[string]any{"device": d.name, "wled_host": d.host, "wled_sub": sub}
		if unit != "" {
			a["unit_of_measurement"] = unit
		}
		if max > 0 {
			a["min"] = min
			a["max"] = max
			a["step"] = 1
		}
		return a
	}

	// on/off
	state := "off"
	if st.On {
		state = "on"
	}
	m.store.Set(entity.Entity{
		ID: "switch." + slug, Name: d.name, Domain: "switch", State: state,
		Attributes: attrs("power", "", 0, 0),
	})

	// brightness
	m.store.Set(entity.Entity{
		ID: "number." + slug + "_brightness", Name: d.name + " Brightness", Domain: "number",
		State: fmt.Sprintf("%d", st.Bri), Attributes: attrs("brightness", "", 0, 255),
	})

	// primary colour (seg[0].col[0]) + effect
	if len(st.Seg) > 0 {
		if len(st.Seg[0].Col) > 0 && len(st.Seg[0].Col[0]) >= 3 {
			r, g, b := st.Seg[0].Col[0][0], st.Seg[0].Col[0][1], st.Seg[0].Col[0][2]
			m.mu.Lock()
			d.r, d.g, d.b = r, g, b
			m.mu.Unlock()
			for _, ch := range []struct {
				sub string
				val int
			}{{"r", r}, {"g", g}, {"b", b}} {
				m.store.Set(entity.Entity{
					ID: "number." + slug + "_" + ch.sub, Name: d.name + " " + strings.ToUpper(ch.sub),
					Domain: "number", State: fmt.Sprintf("%d", ch.val), Attributes: attrs(ch.sub, "", 0, 255),
				})
			}
		}
		m.store.Set(entity.Entity{
			ID: "number." + slug + "_effect", Name: d.name + " Effect", Domain: "number",
			State: fmt.Sprintf("%d", st.Seg[0].Fx), Attributes: attrs("effect", "", 0, 200),
		})
	}

	// signal (read-only)
	m.store.Set(entity.Entity{
		ID: "sensor." + slug + "_signal", Name: d.name + " Signal", Domain: "sensor",
		State: fmt.Sprintf("%d", info.Wifi.Rssi),
		Attributes: map[string]any{"device": d.name, "wled_host": d.host, "unit_of_measurement": "dBm"},
	})
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
	host, _ := e.Attributes["wled_host"].(string)
	name, _ := e.Attributes["device"].(string)
	sub, _ := e.Attributes["wled_sub"].(string)
	if host == "" {
		return // not a WLED entity
	}

	lc := strings.ToLower(service)
	var body map[string]any

	switch {
	case strings.HasSuffix(lc, ".set_value"):
		val, ok := numAny(data["value"])
		if !ok {
			return
		}
		switch sub {
		case "brightness":
			body = map[string]any{"bri": clampInt(int(val), 0, 255)}
		case "effect":
			body = map[string]any{"seg": []any{map[string]any{"id": 0, "fx": int(val)}}}
		case "r", "g", "b":
			m.mu.Lock()
			if d := m.devs[name]; d != nil {
				switch sub {
				case "r":
					d.r = clampInt(int(val), 0, 255)
				case "g":
					d.g = clampInt(int(val), 0, 255)
				case "b":
					d.b = clampInt(int(val), 0, 255)
				}
				body = map[string]any{"seg": []any{map[string]any{"id": 0, "col": [][]int{{d.r, d.g, d.b}}}}}
			}
			m.mu.Unlock()
		}
	case strings.HasSuffix(lc, ".turn_on"):
		body = map[string]any{"on": true}
	case strings.HasSuffix(lc, ".turn_off"):
		body = map[string]any{"on": false}
	case strings.HasSuffix(lc, ".toggle"):
		body = map[string]any{"on": e.State != "on"}
	}

	if body == nil {
		return
	}
	buf, _ := json.Marshal(body)
	resp, err := wledHTTP.Post("http://"+host+"/json/state", "application/json", bytes.NewReader(buf))
	if err != nil {
		slog.Warn("wled: post failed", "entity", entityID, "err", err)
		return
	}
	resp.Body.Close()
	slog.Info("wled: cmd", "entity", entityID, "body", string(buf))
	m.optimistic(e, sub, body)
}

func (m *Manager) optimistic(e entity.Entity, sub string, body map[string]any) {
	if on, ok := body["on"].(bool); ok {
		e.State = "off"
		if on {
			e.State = "on"
		}
		m.store.Set(e)
		return
	}
	switch sub {
	case "brightness":
		if v, ok := body["bri"].(int); ok {
			e.State = fmt.Sprintf("%d", v)
			m.store.Set(e)
		}
	case "r", "g", "b", "effect":
		// let the next poll (≤5s) reflect the segment change
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

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
