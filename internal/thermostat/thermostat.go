// Package thermostat is the HomeForge HVAC brain. It averages temperature
// sensors (read off the HA MQTT broker), holds the desired mode/setpoint/preset
// (persisted across restarts), and pushes them to a LiquidFW on-device thermostat
// (the DrZzs D1-mini relay board) which runs the actual hysteresis + anti-short-
// cycle + deadman fail-safe. The device is the safety-critical control loop; this
// is the smart supervisor. Replaces HA's dual_smart_thermostat + coordination.
package thermostat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	mqttclient "github.com/eclipse/paho.mqtt.golang"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

// EntityID is the climate control entity the UI + automations drive.
const EntityID = "climate.house"

// DeviceSender pushes a signed command to a named LiquidFW device (implemented by
// the liquidfw.Manager). Kept as an interface so this package doesn't depend on
// the integration's internals.
type DeviceSender interface {
	SendToDevice(name string, body map[string]any) error
}

type preset struct{ cool, heat float64 }

// tempSrc is one temperature reading (already in °F) with its freshness + staleness
// limit. A source older than maxAge is dropped from the average so a silent Zigbee
// sensor or a dead LiquidFW probe fails safe instead of feeding a frozen number.
type tempSrc struct {
	f      float64
	ts     time.Time
	maxAge time.Duration
}

func (s tempSrc) fresh() bool { return s.maxAge <= 0 || time.Since(s.ts) <= s.maxAge }

// plausibleF rejects obviously-bad indoor readings. A disconnected/failed HTU21D
// returns 0°C (= 32°F), which is a FRESH but invalid reading the staleness guard
// won't catch — so bound it. Outside this band the reading is treated as a sensor
// fault and ignored, so a dead sensor can never tell the thermostat it's freezing.
func plausibleF(f float64) bool { return f >= 40 && f <= 110 }

type Manager struct {
	cfg    config.ThermostatConfig
	store  *entity.Store
	bus    *bus.Bus
	sender DeviceSender

	devSlug  string // cfg.Device with '-' → '_', for reading the device's echo sensors
	interval time.Duration
	minT     float64
	maxT     float64
	presets  map[string]preset

	mu       sync.Mutex
	mode     string             // off | heat | cool | fan_only
	coolSet  float64            // base cooling setpoint (manual)
	heatSet  float64            // base heating setpoint (manual)
	presetNm string             // "none" or a preset name
	temps    map[string]tempSrc // source key (topic/entity) → latest reading (°F)

	presenceHome bool
	notHomeSince time.Time
	pushedValid  bool // have we pushed a VALID temp to the device since startup / a no-temp gap?

	circulate bool      // currently requesting blower circulation (destratify)
	circStart time.Time // when the current circulation run started
	circStop  time.Time // when the last circulation run ended (for the min-off rest)

	// Provenance of the last setpoint change — who moved it and when. Lets the climate brain
	// honor a manual (UI/assistant) override for an hour before resuming control, instead of
	// guessing (which caused wrong "the brain drifted it" calls). Stamped onto climate.house.
	setpointSource  string    // "user" | "assistant" | "brain" | "" (startup)
	setpointChanged time.Time // when the setpoint last actually changed
}

type persistState struct {
	Mode            string  `json:"mode"`
	CoolSet         float64 `json:"cool_setpoint"`
	HeatSet         float64 `json:"heat_setpoint"`
	Preset          string  `json:"preset"`
	SetpointSource  string  `json:"setpoint_source,omitempty"`
	SetpointChanged int64   `json:"setpoint_changed,omitempty"` // unix seconds
}

func NewManager(cfg config.ThermostatConfig, store *entity.Store, b *bus.Bus, sender DeviceSender) *Manager {
	m := &Manager{
		cfg:      cfg,
		store:    store,
		bus:      b,
		sender:   sender,
		devSlug:  strings.ReplaceAll(strings.ToLower(cfg.Device), "-", "_"),
		interval: time.Duration(orInt(cfg.PushIntervalSec, 30)) * time.Second,
		minT:     orFloat(cfg.MinTemp, 55),
		maxT:     orFloat(cfg.MaxTemp, 88),
		presets:  map[string]preset{},
		temps:    map[string]tempSrc{},
		// Sensible defaults; overwritten by the persisted state if present.
		mode:         "off",
		coolSet:      74,
		heatSet:      70,
		presetNm:     "none",
		presenceHome: true,
	}
	for _, p := range cfg.Presets {
		m.presets[p.Name] = preset{cool: p.Cool, heat: p.Heat}
	}
	m.load()
	return m
}

func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("thermostat: disabled")
		<-ctx.Done()
		return
	}
	slog.Info("thermostat: starting", "device", m.cfg.Device, "mode", m.mode,
		"cool", m.coolSet, "heat", m.heatSet, "preset", m.presetNm)

	m.startMQTT(ctx)

	// UI / automation control: climate.set_hvac_mode / set_temperature / set_preset_mode.
	m.bus.Subscribe("service.call", func(ev bus.Event) {
		p, ok := ev.Payload.(map[string]any)
		if !ok {
			return
		}
		if eid, _ := p["entity"].(string); eid != EntityID {
			return
		}
		svc, _ := p["service"].(string)
		data, _ := p["data"].(map[string]any)
		source, _ := p["source"].(string)
		m.handleService(svc, data, source)
	})

	// Optional presence-driven auto-away.
	if m.cfg.PresenceEntity != "" {
		m.bus.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
			p, ok := ev.Payload.(entity.StateChangedPayload)
			if !ok || p.Entity.ID != m.cfg.PresenceEntity {
				return
			}
			m.onPresence(p.Entity.State)
		})
		if e, ok := m.store.Get(m.cfg.PresenceEntity); ok {
			m.onPresence(e.State)
		}
	}

	// Live UI refresh: whenever a temp probe reports (~5s), re-read the probes and
	// re-publish the climate entity so the tab shows fresh temps and never blanks for
	// the 5-min push interval (notably right after a restart, before the first push
	// tick). This ONLY refreshes the UI entity — the device push stays on the timer.
	tempIDs := map[string]bool{}
	for _, s := range m.cfg.TempSensors {
		if s.Entity != "" {
			tempIDs[s.Entity] = true
		}
	}
	if len(tempIDs) > 0 {
		m.bus.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
			p, ok := ev.Payload.(entity.StateChangedPayload)
			if !ok || !tempIDs[p.Entity.ID] {
				return
			}
			m.readEntitySensors()
			m.mu.Lock()
			_, haveTemp := m.avgTempFLocked()
			needPush := haveTemp && !m.pushedValid // first valid temp after startup / a no-temp gap
			if !haveTemp {
				m.pushedValid = false // temp went stale → re-push the moment a valid reading returns
			}
			circChanged := m.evaluateCirculateLocked(time.Now()) // start/stop blower circulation
			m.mu.Unlock()
			if needPush || circChanged {
				m.pushAndPublish() // push temp / a circulate on/off change to the device promptly
			} else {
				m.publishEntity()
			}
		})
	}

	m.readEntitySensors()
	m.publishEntity() // register immediately so the UI has it before the first push
	m.pushAndPublish()

	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.checkAutoAway()
			m.pushAndPublish()
		}
	}
}

// ── temperature ingest (own MQTT client on the HA broker) ────────────────────

func (m *Manager) startMQTT(ctx context.Context) {
	if m.cfg.MQTTHost == "" || len(m.cfg.TempSensors) == 0 {
		slog.Warn("thermostat: no temp MQTT configured — device will fail-safe (deadman)")
		return
	}
	port := orInt(m.cfg.MQTTPort, 1883)
	opts := mqttclient.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", m.cfg.MQTTHost, port)).
		SetClientID("homeforge-thermostat").
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetOnConnectHandler(func(c mqttclient.Client) {
			for _, s := range m.cfg.TempSensors {
				c.Subscribe(s.Topic, 0, m.onTempMsg)
			}
			slog.Info("thermostat: temp MQTT connected", "host", m.cfg.MQTTHost, "sensors", len(m.cfg.TempSensors))
		})
	if m.cfg.MQTTUser != "" {
		opts.SetUsername(m.cfg.MQTTUser).SetPassword(m.cfg.MQTTPass)
	}
	c := mqttclient.NewClient(opts)
	c.Connect() // async; SetConnectRetry keeps trying
	go func() { <-ctx.Done(); c.Disconnect(200) }()
}

func (m *Manager) onTempMsg(_ mqttclient.Client, msg mqttclient.Message) {
	var p map[string]any
	if json.Unmarshal(msg.Payload(), &p) != nil {
		return
	}
	field, celsius, maxAge, found := "temperature", false, time.Duration(0), false
	for _, s := range m.cfg.TempSensors {
		if s.Topic != "" && s.Topic == msg.Topic() {
			if s.Field != "" {
				field = s.Field
			}
			celsius = s.Celsius
			maxAge = time.Duration(s.MaxAgeSec) * time.Second
			found = true
			break
		}
	}
	if !found {
		return
	}
	v, ok := p[field].(float64)
	if !ok {
		return
	}
	if celsius {
		v = v*9/5 + 32
	}
	if !plausibleF(v) {
		return // sensor fault (e.g. disconnected → 0°C) — ignore
	}
	m.mu.Lock()
	m.temps[msg.Topic()] = tempSrc{f: v, ts: time.Now(), maxAge: maxAge}
	m.mu.Unlock()
}

// readEntitySensors pulls temp readings from HomeForge entities (LiquidFW probes,
// which arrive over UDP as sensor.<name>_climate_temperature). Uses the entity's
// LastUpdated as the freshness timestamp — so an offline probe goes stale and drops
// out of the average. Called on each tick before computing the average.
func (m *Manager) readEntitySensors() {
	for _, s := range m.cfg.TempSensors {
		if s.Entity == "" {
			continue
		}
		e, ok := m.store.Get(s.Entity)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(e.State), 64)
		if err != nil {
			continue
		}
		if s.Celsius {
			f = f*9/5 + 32
		}
		if !plausibleF(f) {
			continue // sensor fault (e.g. unwired probe reads 0°C) — skip
		}
		m.mu.Lock()
		m.temps[s.Entity] = tempSrc{f: f, ts: e.LastUpdated, maxAge: time.Duration(s.MaxAgeSec) * time.Second}
		m.mu.Unlock()
	}
}

// ── control ──────────────────────────────────────────────────────────────────

func (m *Manager) handleService(svc string, data map[string]any, source string) {
	s := svc
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	if source == "" {
		source = "user" // service calls without a tag come from the (auth-gated) API = a human
	}
	m.mu.Lock()
	changed := true
	switch s {
	case "set_hvac_mode":
		if v, ok := data["hvac_mode"].(string); ok {
			m.mode = normMode(v)
		}
	case "set_temperature":
		if v, ok := toFloat(data["temperature"]); ok {
			m.presetNm = "none"
			v = clamp(v, m.minT, m.maxT)
			cur := m.coolSet
			if m.mode == "heat" {
				cur = m.heatSet
				m.heatSet = v
			} else {
				m.coolSet = v
			}
			// Record provenance only on a real change, so re-sending the same value doesn't
			// keep resetting the manual-override clock.
			if v != cur {
				m.setpointSource = source
				m.setpointChanged = time.Now()
			}
		}
	case "set_preset_mode":
		if v, ok := data["preset_mode"].(string); ok {
			m.presetNm = v
		}
	default:
		changed = false
	}
	m.mu.Unlock()
	if changed {
		m.persist()
		m.pushAndPublish()
	}
}

func (m *Manager) onPresence(state string) {
	home := false
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "home", "on", "true", "occupied":
		home = true
	}
	m.mu.Lock()
	was := m.presenceHome
	m.presenceHome = home
	if home {
		m.notHomeSince = time.Time{}
		if m.presetNm == "away" {
			m.presetNm = "home"
		}
	} else if was {
		m.notHomeSince = time.Now()
	}
	m.mu.Unlock()
	if home != was {
		m.persist()
		m.pushAndPublish()
	}
}

// checkAutoAway flips to the away preset once the house has been empty long enough.
func (m *Manager) checkAutoAway() {
	if m.cfg.PresenceEntity == "" {
		return
	}
	after := time.Duration(orInt(m.cfg.AwayAfterMin, 15)) * time.Minute
	m.mu.Lock()
	trip := !m.presenceHome && !m.notHomeSince.IsZero() &&
		time.Since(m.notHomeSince) >= after && m.presetNm != "away"
	if trip {
		m.presetNm = "away"
	}
	m.mu.Unlock()
	if trip {
		slog.Info("thermostat: auto-away")
		m.persist()
	}
}

// pushAndPublish sends the current desired state to the device and refreshes the
// climate entity. Called on the timer and on every state change.
func (m *Manager) pushAndPublish() {
	m.readEntitySensors() // refresh LiquidFW-probe temps from the store before averaging
	m.mu.Lock()
	mode := m.mode
	cool, heat := m.effectiveSetpointsLocked()
	temp, haveTemp := m.avgTempFLocked()
	m.pushedValid = haveTemp // record whether the device just got a valid temp
	m.evaluateCirculateLocked(time.Now())
	circ := m.circulate
	m.mu.Unlock()

	setpoint := cool
	if mode == "heat" {
		setpoint = heat
	}

	body := map[string]any{
		"thermostat_mode":      mode,
		"thermostat_setpoint":  round1(setpoint),
		"thermostat_circulate": circ,
	}
	if haveTemp {
		body["thermostat_temp"] = round1(temp)
	}
	if err := m.sender.SendToDevice(m.cfg.Device, body); err != nil {
		slog.Debug("thermostat: push failed", "device", m.cfg.Device, "err", err)
	}
	m.publishEntity()
}

func (m *Manager) publishEntity() {
	m.mu.Lock()
	mode, presetNm := m.mode, m.presetNm
	cool, heat := m.effectiveSetpointsLocked()
	temp, haveTemp := m.avgTempFLocked()
	delta, haveDelta := m.deltaFLocked()
	circ := m.circulate
	spSource, spChanged := m.setpointSource, m.setpointChanged
	perSensor := map[string]any{}
	for key, r := range m.temps {
		if r.fresh() {
			perSensor[key] = round1(r.f)
		}
	}
	m.mu.Unlock()

	setpoint := cool
	if mode == "heat" {
		setpoint = heat
	}

	// hvac_action comes from the device's echoed status (idle/heating/cooling/fan/failsafe).
	action := "unknown"
	if e, ok := m.store.Get("sensor." + m.devSlug + "_thermostat_action"); ok {
		action = e.State
	}

	attrs := map[string]any{
		"friendly_name":  "House",
		"temperature":    round1(setpoint),
		"cool_setpoint":  round1(cool),
		"heat_setpoint":  round1(heat),
		"preset_mode":    presetNm,
		"hvac_action":    action,
		"hvac_modes":     []string{"off", "cool", "heat", "fan_only"},
		"preset_modes":   m.presetNames(),
		"min_temp":       m.minT,
		"max_temp":       m.maxT,
		"unit":           "°F",
		"device":         m.cfg.Device,
		"temp_sensors":   perSensor,
		"temp_available": haveTemp,
		"circulating":    circ,
	}
	// Setpoint provenance (who last moved it + when) so the climate brain can honor a manual override.
	if spSource != "" {
		attrs["setpoint_source"] = spSource
	}
	if !spChanged.IsZero() {
		attrs["setpoint_changed"] = spChanged.Unix()
	}
	if haveTemp {
		attrs["current_temperature"] = round1(temp)
	}
	if haveDelta {
		attrs["updown_delta"] = round1(delta)
	}
	m.store.Set(entity.Entity{
		ID:         EntityID,
		Name:       "House",
		Domain:     "climate",
		State:      mode,
		Attributes: attrs,
	})
	// Observability entities for the dashboard + history.
	circState := "off"
	if circ {
		circState = "on"
	}
	m.store.Set(entity.Entity{
		ID: "binary_sensor.climate_circulating", Name: "Climate Circulating", Domain: "binary_sensor",
		State: circState, Attributes: map[string]any{"device": "climate-control", "section": "climate"},
	})
	if haveDelta {
		m.store.Set(entity.Entity{
			ID: "sensor.climate_updown_delta", Name: "Up/Down Temp Delta", Domain: "sensor",
			State:      strconv.FormatFloat(delta, 'f', 1, 64),
			Attributes: map[string]any{"device": "climate-control", "section": "climate", "unit_of_measurement": "°F"},
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (m *Manager) effectiveSetpointsLocked() (cool, heat float64) {
	if p, ok := m.presets[m.presetNm]; ok {
		return p.cool, p.heat
	}
	return m.coolSet, m.heatSet
}

func (m *Manager) avgTempFLocked() (float64, bool) {
	var sum float64
	n := 0
	for _, r := range m.temps {
		if !r.fresh() {
			continue // stale source (silent sensor / offline probe) — drop it
		}
		sum += r.f
		n++
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// deltaFLocked = the stratification spread (hottest − coldest) across the fresh probes.
func (m *Manager) deltaFLocked() (float64, bool) {
	lo, hi, n := 0.0, 0.0, 0
	for _, r := range m.temps {
		if !r.fresh() {
			continue
		}
		if n == 0 || r.f < lo {
			lo = r.f
		}
		if n == 0 || r.f > hi {
			hi = r.f
		}
		n++
	}
	if n < 2 {
		return 0, false
	}
	return hi - lo, true
}

// evaluateCirculateLocked updates m.circulate from the floor-to-floor delta with hysteresis
// + min-off/max-run guards. HF only decides WHETHER to circulate; the device runs the blower
// only when it's idle (not heating/cooling). Returns true if the request flag changed.
func (m *Manager) evaluateCirculateLocked(now time.Time) bool {
	prev := m.circulate
	c := m.cfg.Circulate
	if !c.Enabled {
		m.circulate = false
		return prev != m.circulate
	}
	on, off := c.OnDelta, c.OffDelta
	if on <= 0 {
		on = 3.0
	}
	if off <= 0 {
		off = 1.5
	}
	maxRun := time.Duration(orInt(c.MaxRunMin, 15)) * time.Minute
	minOff := time.Duration(orInt(c.MinOffMin, 5)) * time.Minute

	delta, ok := m.deltaFLocked()
	if !ok { // need both probes fresh to judge stratification
		if m.circulate {
			m.circulate, m.circStop = false, now
		}
		return prev != m.circulate
	}
	if m.circulate {
		if delta <= off || now.Sub(m.circStart) >= maxRun {
			m.circulate, m.circStop = false, now
		}
	} else {
		if delta >= on && now.Sub(m.circStop) >= minOff {
			m.circulate, m.circStart = true, now
		}
	}
	return prev != m.circulate
}

func (m *Manager) presetNames() []string {
	out := []string{"none"}
	for _, p := range m.cfg.Presets {
		out = append(out, p.Name)
	}
	return out
}

func (m *Manager) stateFile() string {
	if m.cfg.StateFile != "" {
		return m.cfg.StateFile
	}
	return "/data/thermostat.json"
}

func (m *Manager) persist() {
	m.mu.Lock()
	ps := persistState{Mode: m.mode, CoolSet: m.coolSet, HeatSet: m.heatSet, Preset: m.presetNm,
		SetpointSource: m.setpointSource}
	if !m.setpointChanged.IsZero() {
		ps.SetpointChanged = m.setpointChanged.Unix()
	}
	m.mu.Unlock()
	data, _ := json.MarshalIndent(ps, "", "  ")
	if err := os.WriteFile(m.stateFile(), data, 0644); err != nil {
		slog.Warn("thermostat: persist failed", "err", err)
	}
}

func (m *Manager) load() {
	data, err := os.ReadFile(m.stateFile())
	if err != nil {
		return
	}
	var ps persistState
	if json.Unmarshal(data, &ps) != nil {
		return
	}
	if ps.Mode != "" {
		m.mode = ps.Mode
	}
	if ps.CoolSet != 0 {
		m.coolSet = ps.CoolSet
	}
	if ps.HeatSet != 0 {
		m.heatSet = ps.HeatSet
	}
	if ps.Preset != "" {
		m.presetNm = ps.Preset
	}
	if ps.SetpointSource != "" {
		m.setpointSource = ps.SetpointSource
	}
	if ps.SetpointChanged != 0 {
		m.setpointChanged = time.Unix(ps.SetpointChanged, 0)
	}
	slog.Info("thermostat: restored state", "mode", m.mode, "cool", m.coolSet, "heat", m.heatSet, "preset", m.presetNm)
}

func normMode(v string) string {
	switch strings.ToLower(v) {
	case "heat":
		return "heat"
	case "cool":
		return "cool"
	case "fan", "fan_only":
		return "fan_only"
	default:
		return "off"
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}
