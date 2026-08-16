// Package wiz is a self-contained HomeForge integration for Philips WiZ bulbs.
//
// WiZ bulbs speak plain UDP JSON on port 38899. This integration:
//   - discovers bulbs by broadcasting getPilot on every homeforge subnet
//     (the hub is dual-homed VLAN1 + VLAN20, so both are swept),
//   - polls getPilot to keep state fresh,
//   - accepts homeforge service calls (switch on/off/toggle + number set_value
//     for brightness / color-temp / r,g,b) and sends setPilot.
//
// Entities are modelled the same way as the LiquidFW neolight: one device per
// bulb, with switch.<name> + number.<name>_{brightness,temp,r,g,b}. Everything
// lives in the entity store and the homeforge UI/websocket — WiZ never touches
// MQTT or Home Assistant.
package wiz

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

const (
	wizPort       = 38899
	discoverEvery = 20 * time.Second
	pollEvery     = 5 * time.Second
	udpTimeout    = 800 * time.Millisecond
	discoverWait  = 1500 * time.Millisecond
)

// wizBroadcasts returns a directed-broadcast address for each local IPv4 subnet, so WiZ
// discovery works on any network with no hardcoded IPs (plus the global broadcast as fallback).
func wizBroadcasts() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			ip, mask := ipnet.IP.To4(), ipnet.Mask
			bc := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bc[i] = ip[i] | ^mask[i]
			}
			out = append(out, bc.String())
		}
	}
	return append(out, "255.255.255.255")
}

// bulb is the cached view of one physical WiZ light.
type bulb struct {
	mac  string
	ip   string
	name string // "wiz_<last6 of mac>" — matches the existing WiZ default names
	// last-known colour channels: WiZ setPilot needs a full r/g/b triple, so a
	// single-channel slider change re-sends the other two from here.
	r, g, b int
	hasRGB  bool
	hasTemp bool
}

// Manager is the WiZ integration entry point.
type Manager struct {
	cfg   config.WiZConfig
	store *entity.Store
	bus   *bus.Bus

	names map[string]string // mac (no colons, lowercase) -> friendly name override

	mu    sync.Mutex
	bulbs map[string]*bulb // keyed by mac
}

func NewManager(cfg config.WiZConfig, store *entity.Store, b *bus.Bus) *Manager {
	// Build the mac->name override map from config. Names are aligned from HA so
	// bulbs come in as e.g. "kitchen-sink" instead of "wiz_3b36bf".
	names := make(map[string]string)
	for _, sb := range cfg.Bulbs {
		if sb.Mac != "" && sb.Name != "" {
			names[normMac(sb.Mac)] = sb.Name
		}
	}
	return &Manager{cfg: cfg, store: store, bus: b, bulbs: make(map[string]*bulb), names: names}
}

// nameFor returns the configured friendly name for a bulb, or the default
// wiz_<last6 of mac> when none is mapped.
func (m *Manager) nameFor(mac string) string {
	if n, ok := m.names[mac]; ok {
		return n
	}
	return "wiz_" + last6(mac)
}

// Run drives discovery + polling and blocks until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("wiz: disabled")
		<-ctx.Done()
		return
	}
	slog.Info("wiz: starting", "seeds", len(m.cfg.Bulbs))

	// Seed any statically-configured bulbs (IP only; mac/name fill on first poll).
	for _, sb := range m.cfg.Bulbs {
		if sb.IP != "" {
			m.pollOne(sb.IP)
		}
	}

	// Commands: subscribe to the shared service.call bus, act only on our own
	// entities (identified by the wiz_mac attribute) and ignore everything else.
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

	// Discovery loop (immediate sweep, then every discoverEvery).
	go func() {
		m.discover()
		t := time.NewTicker(discoverEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.discover()
			}
		}
	}()

	// Poll loop.
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

// ---- discovery ----

// discover broadcasts getPilot on every subnet and ingests all replies.
func (m *Manager) discover() {
	req := []byte(`{"method":"getPilot","params":{}}`)
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		slog.Warn("wiz: discover listen", "err", err)
		return
	}
	defer pc.Close()

	// Enable SO_BROADCAST on the socket (Go leaves it off by default).
	if raw, err := pc.SyscallConn(); err == nil {
		_ = raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
	}

	for _, addr := range wizBroadcasts() {
		_, _ = pc.WriteToUDP(req, &net.UDPAddr{IP: net.ParseIP(addr), Port: wizPort})
	}

	_ = pc.SetReadDeadline(time.Now().Add(discoverWait))
	buf := make([]byte, 2048)
	for {
		n, src, err := pc.ReadFromUDP(buf)
		if err != nil {
			break // read deadline reached
		}
		m.ingest(buf[:n], src.IP.String())
	}
}

// ---- polling ----

func (m *Manager) pollAll() {
	m.mu.Lock()
	ips := make([]string, 0, len(m.bulbs))
	for _, b := range m.bulbs {
		ips = append(ips, b.ip)
	}
	m.mu.Unlock()
	for _, ip := range ips {
		m.pollOne(ip)
	}
}

func (m *Manager) pollOne(ip string) {
	resp, err := query(ip, []byte(`{"method":"getPilot","params":{}}`))
	if err != nil {
		return
	}
	m.ingest(resp, ip)
}

// query sends one JSON request to a bulb and returns its raw reply.
func query(ip string, req []byte) ([]byte, error) {
	conn, err := net.DialTimeout("udp4", fmt.Sprintf("%s:%d", ip, wizPort), udpTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(udpTimeout))
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// ---- parse + publish ----

// ingest parses a getPilot reply, updates the cached bulb, and publishes entities.
func (m *Manager) ingest(raw []byte, srcIP string) {
	var env struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Result == nil {
		return
	}
	res := env.Result
	macRaw, ok := res["mac"]
	if !ok {
		return
	}
	mac := normMac(fmt.Sprint(macRaw))
	if mac == "" {
		return
	}
	name := m.nameFor(mac)

	m.mu.Lock()
	b, ok := m.bulbs[mac]
	if !ok {
		b = &bulb{mac: mac, name: name}
		m.bulbs[mac] = b
		slog.Info("wiz: bulb discovered", "name", name, "ip", srcIP)
	}
	b.ip = srcIP
	b.name = name
	if _, hr := res["r"]; hr {
		b.hasRGB = true
	}
	if _, hg := res["g"]; hg {
		b.hasRGB = true
	}
	if _, hb := res["b"]; hb {
		b.hasRGB = true
	}
	if b.hasRGB {
		if v, ok := numOf(res, "r"); ok {
			b.r = int(v)
		}
		if v, ok := numOf(res, "g"); ok {
			b.g = int(v)
		}
		if v, ok := numOf(res, "b"); ok {
			b.b = int(v)
		}
	}
	if _, ht := res["temp"]; ht {
		b.hasTemp = true
	}
	snapshot := *b
	m.mu.Unlock()

	m.publish(snapshot, res)
}

// publish writes the bulb's entities into the store.
func (m *Manager) publish(b bulb, res map[string]any) {
	slug := slugify(b.name) // entity-id-safe form of the friendly name

	attrs := func(sub, unit string, min, max int) map[string]any {
		a := map[string]any{
			"device":  b.name,
			"wiz_mac": b.mac,
			"wiz_ip":  b.ip,
			"wiz_sub": sub,
		}
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
	if bv, ok := res["state"].(bool); ok && bv {
		state = "on"
	}
	m.store.Set(entity.Entity{
		ID:         "switch." + slug,
		Name:       b.name,
		Domain:     "switch",
		State:      state,
		Attributes: attrs("power", "", 0, 0),
	})

	// brightness (WiZ dimming is 10-100)
	if v, ok := numOf(res, "dimming"); ok {
		m.store.Set(entity.Entity{
			ID:         "number." + slug + "_brightness",
			Name:       b.name + " Brightness",
			Domain:     "number",
			State:      fmt.Sprintf("%d", int(v)),
			Attributes: attrs("brightness", "%", 10, 100),
		})
	}

	// colour temperature (tunable-white bulbs only)
	if b.hasTemp {
		if v, ok := numOf(res, "temp"); ok {
			m.store.Set(entity.Entity{
				ID:         "number." + slug + "_temp",
				Name:       b.name + " Color Temp",
				Domain:     "number",
				State:      fmt.Sprintf("%d", int(v)),
				Attributes: attrs("temp", "K", 2200, 6500),
			})
		}
	}

	// r/g/b (RGB bulbs only)
	if b.hasRGB {
		for _, ch := range []struct {
			sub string
			val int
		}{{"r", b.r}, {"g", b.g}, {"b", b.b}} {
			m.store.Set(entity.Entity{
				ID:         "number." + slug + "_" + ch.sub,
				Name:       b.name + " " + strings.ToUpper(ch.sub),
				Domain:     "number",
				State:      fmt.Sprintf("%d", ch.val),
				Attributes: attrs(ch.sub, "", 0, 255),
			})
		}
	}

	// signal strength (device-level; no wiz_sub so it's read-only in the UI)
	if v, ok := numOf(res, "rssi"); ok {
		m.store.Set(entity.Entity{
			ID:     "sensor." + slug + "_signal",
			Name:   b.name + " Signal",
			Domain: "sensor",
			State:  fmt.Sprintf("%d", int(v)),
			Attributes: map[string]any{
				"device":              b.name,
				"wiz_mac":             b.mac,
				"unit_of_measurement": "dBm",
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
	mac, _ := e.Attributes["wiz_mac"].(string)
	ip, _ := e.Attributes["wiz_ip"].(string)
	sub, _ := e.Attributes["wiz_sub"].(string)
	if mac == "" || ip == "" {
		return // not a WiZ entity — ignore
	}

	lc := strings.ToLower(service)
	var params map[string]any

	switch {
	case strings.HasSuffix(lc, ".set_value"):
		val, ok := numAny(data["value"])
		if !ok {
			return
		}
		switch sub {
		case "brightness":
			params = map[string]any{"dimming": clampInt(int(val), 10, 100)}
		case "temp":
			params = map[string]any{"temp": clampInt(int(val), 2200, 6500)}
		case "r", "g", "b":
			m.mu.Lock()
			if b := m.bulbs[mac]; b != nil {
				switch sub {
				case "r":
					b.r = clampInt(int(val), 0, 255)
				case "g":
					b.g = clampInt(int(val), 0, 255)
				case "b":
					b.b = clampInt(int(val), 0, 255)
				}
				params = map[string]any{"r": b.r, "g": b.g, "b": b.b}
			}
			m.mu.Unlock()
		}
	case strings.HasSuffix(lc, ".turn_on"):
		params = map[string]any{"state": true}
	case strings.HasSuffix(lc, ".turn_off"):
		params = map[string]any{"state": false}
	case strings.HasSuffix(lc, ".toggle"):
		params = map[string]any{"state": e.State != "on"}
	}

	if params == nil {
		return
	}

	body, _ := json.Marshal(map[string]any{"method": "setPilot", "params": params})
	m.optimistic(e, sub, params) // reflect intent immediately (UI + automation idempotency)
	// WiZ bulbs drop ~10% of UDP packets and HF gets no ack on a lost one, so a single
	// send can silently miss (light doesn't switch). Retry up to 3× (a real miss -> ~0.1%).
	// Async so a slow/dead bulb never blocks the shared service bus.
	go func() {
		for attempt := 1; attempt <= 3; attempt++ {
			if _, err := query(ip, body); err == nil {
				slog.Info("wiz: cmd", "entity", entityID, "params", params, "attempt", attempt)
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
		slog.Warn("wiz: setPilot failed after 3 tries", "entity", entityID)
	}()
}

// optimistic updates the store immediately so the UI reacts before the next poll.
func (m *Manager) optimistic(e entity.Entity, sub string, params map[string]any) {
	if st, ok := params["state"].(bool); ok {
		e.State = "off"
		if st {
			e.State = "on"
		}
		m.store.Set(e)
		return
	}
	var v any
	switch sub {
	case "brightness":
		v = params["dimming"]
	case "temp":
		v = params["temp"]
	case "r", "g", "b":
		v = params[sub]
	}
	if v != nil {
		e.State = fmt.Sprintf("%d", toInt(v))
		m.store.Set(e)
	}
}

// ---- helpers ----

func normMac(mac string) string {
	return strings.ToLower(strings.ReplaceAll(mac, ":", ""))
}

func last6(mac string) string {
	s := strings.ReplaceAll(mac, ":", "")
	if len(s) >= 6 {
		return s[len(s)-6:]
	}
	return s
}

// slugify converts a friendly name to an entity-id-safe token: lowercase, with
// every run of non-alphanumeric characters collapsed to a single underscore.
// "kitchen-sink" -> "kitchen_sink", "Back Patio Left" -> "back_patio_left".
func slugify(s string) string {
	var b strings.Builder
	prevUnder := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnder = false
		} else if !prevUnder {
			b.WriteByte('_')
			prevUnder = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func numOf(m map[string]any, k string) (float64, bool) {
	v, ok := m[k]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
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

func toInt(v any) int {
	if f, ok := numAny(v); ok {
		return int(f)
	}
	return 0
}
