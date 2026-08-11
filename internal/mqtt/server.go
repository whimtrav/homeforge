package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mqttclient "github.com/eclipse/paho.mqtt.golang"
	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
	"github.com/whimtrav/homeforge/internal/push"
)

const TopicMQTTMessage = "mqtt.message"

type Message struct {
	Topic   string
	Payload []byte
}

// ringBinding maps a ring/ state topic to an entity + its value_template (multiple entities
// can share one topic, e.g. battery + wifi both read the device's info/state JSON).
type ringBinding struct {
	entityID string
	tmpl     string
}

type Server struct {
	cfg    config.MQTTConfig
	store  *entity.Store
	bus    *bus.Bus
	broker *mqttserver.Server
	client mqttclient.Client

	ringMu       sync.Mutex
	ringBindings map[string][]ringBinding // state_topic -> entities that read it
}

func NewServer(cfg config.MQTTConfig, store *entity.Store, b *bus.Bus) (*Server, error) {
	s := &Server{cfg: cfg, store: store, bus: b, ringBindings: map[string][]ringBinding{}}

	// Handle service calls for MQTT-backed entities (Zigbee2MQTT, Tasmota).
	b.Subscribe("service.call", func(ev bus.Event) {
		payload, ok := ev.Payload.(map[string]any)
		if !ok {
			return
		}
		entityID, _ := payload["entity"].(string)
		service, _ := payload["service"].(string)
		data, _ := payload["data"].(map[string]any)
		s.handleServiceCallMQTT(entityID, service, data)
	})

	if !cfg.External {
		broker := mqttserver.New(&mqttserver.Options{})
		if err := broker.AddHook(new(auth.AllowHook), nil); err != nil {
			return nil, fmt.Errorf("mqtt: add auth hook: %w", err)
		}
		addr := fmt.Sprintf(":%d", cfg.Port)
		if cfg.Port == 0 {
			addr = ":1883"
		}
		if err := broker.AddListener(listeners.NewTCP(listeners.Config{Address: addr})); err != nil {
			return nil, fmt.Errorf("mqtt: add listener: %w", err)
		}
		s.broker = broker
		slog.Info("mqtt: embedded broker configured", "addr", addr)
	}

	return s, nil
}

func (s *Server) Run(ctx context.Context) {
	if s.broker != nil {
		go func() {
			if err := s.broker.Serve(); err != nil {
				slog.Error("mqtt: broker error", "err", err)
			}
		}()
		slog.Info("mqtt: embedded broker started")
	}

	host := s.cfg.Host
	if host == "" {
		host = "localhost"
	}
	port := s.cfg.Port
	if port == 0 {
		port = 1883
	}
	broker := fmt.Sprintf("tcp://%s:%d", host, port)

	opts := mqttclient.NewClientOptions().
		AddBroker(broker).
		SetClientID("homeforge").
		SetAutoReconnect(true).
		SetMessageChannelDepth(4096). // absorb bursts (ring-mqtt resends ~1800 discovery msgs at once)
		SetOnConnectHandler(s.onConnect)

	if s.cfg.Username != "" {
		opts.SetUsername(s.cfg.Username).SetPassword(s.cfg.Password)
	}

	client := mqttclient.NewClient(opts)
	s.client = client

	if token := client.Connect(); token.Wait() && token.Error() != nil {
		slog.Error("mqtt: client connect failed", "err", token.Error())
	}

	<-ctx.Done()
	client.Disconnect(500)
	if s.broker != nil {
		_ = s.broker.Close()
	}
	slog.Info("mqtt: stopped")
}

func (s *Server) onConnect(c mqttclient.Client) {
	slog.Info("mqtt: client connected")

	// HA MQTT discovery messages are retained — seed entities from them
	// immediately on connect. Device state updates arrive on zigbee2mqtt/+.
	c.Subscribe("homeassistant/#", 0, s.handleHADiscovery)

	// Zigbee2MQTT live device state updates.
	c.Subscribe("zigbee2mqtt/+", 0, s.handleZigbee2MQTT)
	// Multi-level topics (e.g. groups, scenes).
	c.Subscribe("zigbee2mqtt/+/+", 0, s.handleZigbee2MQTT)

	// Tasmota stat topics.
	c.Subscribe("stat/+/RESULT", 0, s.handleTasmota)
	c.Subscribe("stat/+/POWER", 0, s.handleTasmota)
	c.Subscribe("tele/+/SENSOR", 0, s.handleTasmota)
	c.Subscribe("tele/+/STATE", 0, s.handleTasmota)

	// Sentinel NVR detection events.
	c.Subscribe("frigate/events", 0, s.handleSentinel) // Sentinel/Frigate detection event stream (JSON)

	// Solar Assistant telemetry, bridged in from its mosquitto (solar_assistant/{dev}/{metric}/state).
	c.Subscribe("solar_assistant/#", 0, s.handleSolarAssistant)

	// Emporia Vue per-channel power, published by the emporia sidecar (emporia/{key}/power).
	c.Subscribe("emporia/#", 0, s.handleEmporia)

	// VeSync cloud devices + ESF24 scale, published by the vesync-bridge sidecar (vesync/{dev}/{metric}).
	c.Subscribe("vesync/+/+", 0, s.handleVeSync)

	// Kidde HomeSafe smoke/CO/water detectors, published by the kidde-bridge sidecar (kidde/{dev}/{key}).
	c.Subscribe("kidde/+/+", 0, s.handleKidde)

	// Hubspace (Afero) lights/outlets/fans, published by the hubspace-bridge sidecar (hubspace/{dev}/{key}).
	c.Subscribe("hubspace/+/+", 0, s.handleHubspace)

	// Ring (ring-mqtt sidecar): HA discovery on homeassistant/# seeds entities; ring/# carries state.
	c.Subscribe("ring/#", 0, s.handleRingState)

	// zwave-js-ui door locks: value topics zwave/nodeID_<n>/98/0/<prop> carry state; commands
	// go back out via handleServiceCallMQTT. (HA discovery for zwave is disabled — its generic
	// entities are unusable; this dedicated handler makes clean, controllable lock entities.)
	c.Subscribe("zwave/#", 0, s.handleZwaveState)
	// Purge the junk lock entities the generic HA-discovery path created before this handler.
	for _, id := range []string{"lock.currentmode_nodeid_3_lock", "lock.currentmode_nodeid_4_lock"} {
		s.store.Delete(id)
	}
	// Seed the zwave-js-ui locks so they exist + route to zwave even before a state message
	// arrives. HF's embedded broker drops retained messages on restart, so without this Ring
	// would re-seed these ids as source "ring" and commands would go to the dead Ring path.
	// Last-known state comes from the boot snapshot; live state follows via handleZwaveState.
	for _, lk := range s.cfg.ZwaveLocks {
		state := "unknown"
		if rs, ok := s.store.Restored("lock." + lk.ID); ok && rs != "" {
			state = rs
		}
		s.store.Set(entity.Entity{ID: "lock." + lk.ID, Name: lk.Name, Domain: "lock", State: state,
			Attributes: map[string]any{"source": "zwavejs", "friendly_name": lk.Name,
				"device": lk.ID, "command_topic": "zwave/" + lk.Node + "/98/0/targetMode/set"}})
	}

	// Hydrific Droplet water sensor — plain MQTT (droplet-<id>/{state,leak,health})
	c.Subscribe("droplet-FE5C/+", 0, s.handleDroplet)

	// Tasmota auto-discovery (retained).
	c.Subscribe("tasmota/discovery/#", 0, s.handleTasmotaDiscovery)

	// LiquidFW device command subscriptions.
	// HA sends: liquidfw/switch.liquidfw_test_led/set {state:on}
	c.Subscribe("liquidfw/+/set", 0, s.handleLiquidFWSet)

	// Forward all messages to automation engine.
	c.Subscribe("#", 0, func(_ mqttclient.Client, msg mqttclient.Message) {
		s.bus.Publish(TopicMQTTMessage, Message{
			Topic:   msg.Topic(),
			Payload: msg.Payload(),
		})
	})

	// Request Z2M to re-publish all device states.
	c.Publish("zigbee2mqtt/bridge/request/devices", 0, false, "{}")

	// Ring: ring-mqtt publishes discovery NON-retained and resends it on the HA "birth"
	// message. Announce online so it re-publishes all Ring device configs to us on every
	// (re)connect — this is how the HA addon works too.
	c.Publish("homeassistant/status", 0, true, "online")
}

// handleHADiscovery parses retained HA MQTT discovery config messages to seed
// entities before any live state arrives. Topic format:
// homeassistant/{domain}/{node_id}/{object_id}/config
func (s *Server) handleHADiscovery(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) < 4 || parts[len(parts)-1] != "config" {
		return
	}
	domain := parts[1]

	var cfg map[string]any
	if err := json.Unmarshal(msg.Payload(), &cfg); err != nil {
		return
	}

	// Ring (ring-mqtt) uses device-based discovery with topics under ring/ — route it to a
	// dedicated handler so its many devices get clean, non-colliding entities (the generic
	// path below derives the name from the state_topic tail, which is "state" for every Ring
	// device → collisions).
	if st, _ := cfg["state_topic"].(string); strings.HasPrefix(st, "ring/") {
		s.handleRingDiscovery(domain, cfg, st)
		return
	}

	switch domain {
	case "sensor", "binary_sensor", "light", "switch", "lock", "climate", "cover":
	default:
		return
	}

	name, _ := cfg["name"].(string)
	if name == "" {
		return
	}

	// Extract device friendly name from state_topic: "zigbee2mqtt/FriendlyName"
	stateTopic, _ := cfg["state_topic"].(string)
	friendlyName := ""
	if stateTopic != "" {
		tp := strings.Split(stateTopic, "/")
		if len(tp) >= 2 {
			friendlyName = tp[len(tp)-1]
		}
	}
	if friendlyName == "" || friendlyName == "bridge" {
		return
	}

	// Build a human-readable entity ID from device name + attribute name.
	// e.g. light.hallway or sensor.hallway_temperature
	attrName := sanitizeID(name)
	deviceID := sanitizeID(friendlyName)

	var id string
	if attrName == deviceID || attrName == "" {
		id = domain + "." + deviceID
	} else {
		id = domain + "." + deviceID + "_" + attrName
	}

	// Don't overwrite an entity that already has real state.
	if existing, exists := s.store.Get(id); exists && existing.State != "unknown" {
		return
	}

	s.store.Set(entity.Entity{
		ID:     id,
		Name:   friendlyName + " " + name,
		Domain: domain,
		State:  "unknown",
		Attributes: map[string]any{
			"friendly_name": friendlyName,
			"state_topic":   stateTopic,
		},
	})
}

func (s *Server) handleZigbee2MQTT(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) < 2 {
		return
	}
	// Skip bridge control/response topics.
	if parts[1] == "bridge" {
		return
	}
	deviceName := parts[1]

	var payload map[string]any
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		return
	}
	if len(payload) == 0 {
		return
	}

	// Determine domain and state from common Zigbee2MQTT fields.
	domain, state := inferDomainState(payload)
	id := domain + "." + sanitizeID(deviceName)

	payload["z2m_topic"] = deviceName
	s.store.Set(entity.Entity{
		ID:         id,
		Name:       deviceName,
		Domain:     domain,
		State:      state,
		Attributes: payload,
	})
}

func (s *Server) handleTasmota(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) < 3 {
		return
	}
	deviceName := parts[1]
	msgType := parts[len(parts)-1]

	var payload map[string]any
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		// Plain text POWER payload ("ON"/"OFF")
		state := strings.ToLower(strings.TrimSpace(string(msg.Payload())))
		id := "switch." + sanitizeID(deviceName)
		s.store.Set(entity.Entity{
			ID:     id,
			Name:   deviceName,
			Domain: "switch",
			State:  state,
		})
		return
	}

	_ = msgType
	if power, ok := payload["POWER"].(string); ok {
		id := "switch." + sanitizeID(deviceName)
		payload["tasmota_topic"] = deviceName
		s.store.Set(entity.Entity{
			ID:         id,
			Name:       deviceName,
			Domain:     "switch",
			State:      strings.ToLower(power),
			Attributes: payload,
		})
	}
}

// handleSentinel ingests Sentinel's Frigate-compatible detection event stream:
//
//	frigate/events → {"type":"new"|"update"|"end","after":{"camera","label",...}}
//
// → binary_sensor.sentinel_{camera}_{label} = on while a detection is active, off on "end".
// Good enough for "person/car at <camera>" automations. (The full JSON is kept in attributes.)
func (s *Server) handleSentinel(_ mqttclient.Client, msg mqttclient.Message) {
	var ev struct {
		Type  string `json:"type"`
		After struct {
			Camera   string  `json:"camera"`
			Label    string  `json:"label"`
			TopScore float64 `json:"top_score"`
		} `json:"after"`
	}
	if json.Unmarshal(msg.Payload(), &ev) != nil || ev.After.Camera == "" || ev.After.Label == "" {
		return
	}
	state := "on"
	if ev.Type == "end" {
		state = "off"
	}
	s.store.Set(entity.Entity{
		ID:     fmt.Sprintf("binary_sensor.sentinel_%s_%s", sanitizeID(ev.After.Camera), sanitizeID(ev.After.Label)),
		Name:   fmt.Sprintf("%s %s", ev.After.Camera, ev.After.Label),
		Domain: "binary_sensor",
		State:  state,
		Attributes: map[string]any{
			"device": "sentinel", "section": "camera",
			"camera": ev.After.Camera, "object": ev.After.Label, "score": ev.After.TopScore,
		},
	})
}

// handleSolarAssistant ingests Solar Assistant telemetry (bridged from its mosquitto).
// Topic: solar_assistant/{device}/{metric}/state  payload = value.
// Only numeric values are kept — that neatly drops the schedule/mode/boolean config topics
// (charge slots, "Voltage", true/false, times) and keeps live power/voltage/current/soc/etc.
func (s *Server) handleSolarAssistant(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) != 4 || parts[3] != "state" {
		return
	}
	val := strings.TrimSpace(string(msg.Payload()))
	if _, err := strconv.ParseFloat(val, 64); err != nil {
		return // non-numeric = config/schedule topic, skip
	}
	metric := parts[2]
	s.store.Set(entity.Entity{
		ID:     "sensor.solar_" + sanitizeID(metric),
		Name:   "solar " + strings.ReplaceAll(metric, "_", " "),
		Domain: "sensor",
		State:  val,
		Attributes: map[string]any{
			"source":  "solar_assistant",
			"metric":  metric,
			"section": "solar",
		},
	})
}

// handleEmporia ingests Emporia Vue per-channel power from the sidecar.
// Topic: emporia/{key}/power  payload = watts. "mains"/"balance" -> mains section
// (kept SEPARATE from the Lux inverter's grid reading in the solar section).
// kiddeBinary maps Kidde binary keys to a HA device_class ("" = none). Anything
// published on kidde/{dev}/{key} not in this map is treated as a plain sensor.
var kiddeBinary = map[string]string{
	"smoke_alarm":       "smoke",
	"co_alarm":          "carbon_monoxide",
	"hardwire_smoke":    "smoke",
	"too_much_smoke":    "smoke",
	"water_alarm":       "moisture",
	"low_temp_alarm":    "cold",
	"low_battery_alarm": "battery",
	"smoke_hushed":      "",
	"contact_lost":      "",
	"online":            "connectivity",
}

// kiddeUnits maps Kidde sensor keys to a unit of measurement.
var kiddeUnits = map[string]string{
	"batt_volt": "V", "battery_voltage": "V", "battery_level": "%",
	"temperature": "°F", "iaq_temperature": "°C", "humidity": "%", "hpa": "hPa",
	"tvoc": "ppb", "co2": "ppm", "ap_rssi": "dBm", "life": "weeks",
}

// handleKidde ingests Kidde HomeSafe detector state from the kidde-bridge sidecar
// (kidde/{device}/{key} = value). Alarm/connectivity keys → binary_sensor, the rest
// → sensor. Read-only (test/hush stay in the Kidde app). Mirrors handleVeSync.
func (s *Server) handleKidde(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) != 3 {
		return
	}
	dev, key := parts[1], parts[2]
	val := strings.TrimSpace(string(msg.Payload()))
	if val == "" {
		return
	}
	devName := strings.ReplaceAll(dev, "_", " ")

	if dc, isBinary := kiddeBinary[key]; isBinary {
		switch strings.ToLower(val) {
		case "on", "true", "1":
			val = "on"
		case "off", "false", "0":
			val = "off"
		}
		attrs := map[string]any{"source": "kidde", "device": dev, "section": "kidde"}
		if dc != "" {
			attrs["device_class"] = dc
		}
		s.store.Set(entity.Entity{
			ID:     "binary_sensor.kidde_" + sanitizeID(dev) + "_" + sanitizeID(key),
			Name:   devName + " " + strings.ReplaceAll(key, "_", " "),
			Domain: "binary_sensor", State: val, Attributes: attrs,
		})
		return
	}

	attrs := map[string]any{"source": "kidde", "device": dev, "metric": key, "section": "kidde"}
	if u, ok := kiddeUnits[key]; ok {
		attrs["unit_of_measurement"] = u
	}
	s.store.Set(entity.Entity{
		ID:     "sensor.kidde_" + sanitizeID(dev) + "_" + sanitizeID(key),
		Name:   "kidde " + devName + " " + strings.ReplaceAll(key, "_", " "),
		Domain: "sensor", State: val, Attributes: attrs,
	})
}

// handleHubspace ingests Hubspace (Afero) device state from the hubspace-bridge
// sidecar (hubspace/{device}/{key} = value). power → switch, brightness/speed →
// number (controllable via handleServiceCallMQTT's hubspace branch → hubspace/{dev}/set),
// everything else → sensor. Mirrors handleVeSync.
func (s *Server) handleHubspace(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) != 3 {
		return
	}
	dev, key := parts[1], parts[2]
	if key == "set" {
		return // command topic echo
	}
	val := strings.TrimSpace(string(msg.Payload()))
	if val == "" {
		return
	}
	devName := strings.ReplaceAll(dev, "_", " ")

	switch key {
	case "power":
		s.store.Set(entity.Entity{
			ID: "switch.hubspace_" + sanitizeID(dev), Name: devName, Domain: "switch", State: val,
			Attributes: map[string]any{"source": "hubspace", "device": dev, "section": "hubspace",
				"hubspace_dev": dev, "hubspace_cmd": "power"},
		})
		return
	case "brightness":
		s.store.Set(entity.Entity{
			ID: "number.hubspace_" + sanitizeID(dev) + "_brightness", Name: devName + " brightness",
			Domain: "number", State: val,
			Attributes: map[string]any{"source": "hubspace", "device": dev, "section": "hubspace",
				"hubspace_dev": dev, "hubspace_cmd": "brightness", "min": 0, "max": 100, "step": 1,
				"unit_of_measurement": "%"},
		})
		return
	case "speed":
		s.store.Set(entity.Entity{
			ID: "number.hubspace_" + sanitizeID(dev) + "_speed", Name: devName + " speed",
			Domain: "number", State: val,
			Attributes: map[string]any{"source": "hubspace", "device": dev, "section": "hubspace",
				"hubspace_dev": dev, "hubspace_cmd": "speed", "min": 0, "max": 100, "step": 1,
				"unit_of_measurement": "%"},
		})
		return
	}

	attrs := map[string]any{"source": "hubspace", "device": dev, "metric": key, "section": "hubspace"}
	if key == "wifi_rssi" {
		attrs["unit_of_measurement"] = "dBm"
	}
	s.store.Set(entity.Entity{
		ID:     "sensor.hubspace_" + sanitizeID(dev) + "_" + sanitizeID(key),
		Name:   "hubspace " + devName + " " + strings.ReplaceAll(key, "_", " "),
		Domain: "sensor", State: val, Attributes: attrs,
	})
}

func (s *Server) handleEmporia(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) != 3 || parts[2] != "power" {
		return
	}
	val := strings.TrimSpace(string(msg.Payload()))
	if _, err := strconv.ParseFloat(val, 64); err != nil {
		return
	}
	key := parts[1]
	section := "circuits"
	if key == "mains" || key == "balance" {
		section = "mains"
	}
	s.store.Set(entity.Entity{
		ID:     "sensor.emporia_" + sanitizeID(key),
		Name:   "Emporia " + strings.ReplaceAll(key, "_", " "),
		Domain: "sensor",
		State:  val,
		Attributes: map[string]any{
			"source":              "emporia",
			"section":             section,
			"unit_of_measurement": "W",
		},
	})
}

// handleVeSync ingests VeSync cloud device + scale metrics from the vesync-bridge sidecar.
// Topic: vesync/{dev}/{metric}  payload = value. Covers the Levoit air purifier + the two
// humidifiers (live state) and the Etekcity ESF24 Bluetooth scale (weight/bmi/height from the
// last synced weigh-in). Scale weight/bmi ALSO flow to the Health tab via the bridge's
// POST to /api/health; these entities are the device-view mirror.
func (s *Server) handleVeSync(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) != 3 {
		return
	}
	dev, metric := parts[1], parts[2]
	val := strings.TrimSpace(string(msg.Payload()))
	if val == "" {
		return
	}
	devName := strings.ReplaceAll(dev, "_", " ")

	// Control metrics become controllable switch/number entities (dispatched by
	// handleServiceCallMQTT via the vesync_dev/vesync_cmd attributes -> vesync/<dev>/set).
	switch metric {
	case "power":
		s.store.Set(entity.Entity{
			ID: "switch.vesync_" + sanitizeID(dev), Name: devName, Domain: "switch", State: val,
			Attributes: map[string]any{"source": "vesync", "device": dev, "section": "vesync",
				"vesync_dev": dev, "vesync_cmd": "power"},
		})
		return
	case "fan":
		s.store.Set(entity.Entity{
			ID: "number.vesync_" + sanitizeID(dev) + "_fan", Name: devName + " fan", Domain: "number", State: val,
			Attributes: map[string]any{"source": "vesync", "device": dev, "section": "vesync",
				"vesync_dev": dev, "vesync_cmd": "fan", "min": 0, "max": 4, "step": 1, "zero_label": "Auto"},
		})
		return
	case "humidity_target":
		s.store.Set(entity.Entity{
			ID: "number.vesync_" + sanitizeID(dev) + "_humidity_target", Name: devName + " target RH",
			Domain: "number", State: val,
			Attributes: map[string]any{"source": "vesync", "device": dev, "section": "vesync",
				"vesync_dev": dev, "vesync_cmd": "humidity", "min": 40, "max": 80, "step": 5,
				"unit_of_measurement": "%"},
		})
		return
	}

	attrs := map[string]any{
		"source":  "vesync",
		"device":  dev,
		"metric":  metric,
		"section": "vesync",
	}
	switch metric {
	case "weight_lb", "target_weight_lb":
		attrs["unit_of_measurement"] = "lb"
	case "weight_kg":
		attrs["unit_of_measurement"] = "kg"
	case "height_cm":
		attrs["unit_of_measurement"] = "cm"
	case "humidity", "filter_life":
		attrs["unit_of_measurement"] = "%"
	case "air_quality_value", "pm25":
		attrs["unit_of_measurement"] = "µg/m³"
	}
	s.store.Set(entity.Entity{
		ID:         "sensor.vesync_" + sanitizeID(dev) + "_" + sanitizeID(metric),
		Name:       "vesync " + devName + " " + strings.ReplaceAll(metric, "_", " "),
		Domain:     "sensor",
		State:      val,
		Attributes: attrs,
	})
}

// ringValueKey extracts the JSON field from a ring-mqtt value_template, e.g.
// `{{ value_json["batteryLevel"] | default("") }}` or `{{ value_json.wirelessSignal }}`.
var ringValueKey = regexp.MustCompile(`value_json(?:\[["']|\.)([A-Za-z0-9_]+)`)

// handleRingDiscovery seeds a clean entity from a ring-mqtt HA-discovery config and registers
// its state binding. Entity id = device name + component (unique in practice; falls back to
// unique_id). Multiple entities can share one state_topic (info/state → battery, wifi, …).
func (s *Server) handleRingDiscovery(domain string, cfg map[string]any, stateTopic string) {
	switch domain {
	case "binary_sensor", "sensor", "switch", "lock", "alarm_control_panel":
	default:
		return // skip camera/button/number/select for now
	}
	name, _ := cfg["name"].(string)
	uniq, _ := cfg["unique_id"].(string)
	devName := ""
	if dev, ok := cfg["device"].(map[string]any); ok {
		devName, _ = dev["name"].(string)
	}
	// ring-mqtt leaves the primary entity's name empty (HA convention: it inherits the device
	// name). Use the device name alone for that; device+component for the rest.
	slug := ""
	primary := name == "" || strings.EqualFold(name, devName)
	switch {
	case devName != "" && primary:
		slug = devName
	case devName != "" && name != "":
		slug = devName + " " + name
	case uniq != "":
		slug = uniq
	case name != "":
		slug = name
	default:
		return
	}
	id := domain + "." + sanitizeID(slug)
	// zwave-js-ui owns these locks (migrated off Ring); never let Ring seed/clobber them.
	for _, lk := range s.cfg.ZwaveLocks {
		if id == "lock."+lk.ID {
			return
		}
	}
	tmpl, _ := cfg["value_template"].(string)

	s.ringMu.Lock()
	dup := false
	for _, b := range s.ringBindings[stateTopic] {
		if b.entityID == id {
			dup = true
			break
		}
	}
	if !dup {
		s.ringBindings[stateTopic] = append(s.ringBindings[stateTopic], ringBinding{entityID: id, tmpl: tmpl})
	}
	s.ringMu.Unlock()

	if existing, ok := s.store.Get(id); ok && existing.State != "unknown" {
		return // keep live state
	}
	friendly := name
	if devName != "" && primary {
		friendly = devName
	} else if devName != "" {
		friendly = devName + " " + name
	}
	attrs := map[string]any{"source": "ring", "section": "ring", "device": sanitizeID(devName),
		"friendly_name": friendly, "state_topic": stateTopic, "unique_id": uniq}
	if dc, ok := cfg["device_class"].(string); ok && dc != "" {
		attrs["device_class"] = dc
	}
	if u, ok := cfg["unit_of_measurement"].(string); ok && u != "" {
		attrs["unit_of_measurement"] = u
	}
	if ct, ok := cfg["command_topic"].(string); ok && ct != "" {
		attrs["command_topic"] = ct
	}
	s.store.Set(entity.Entity{ID: id, Name: friendly, Domain: domain, State: "unknown", Attributes: attrs})
}

// handleRingState updates Ring entities as state arrives on ring/# topics.
func (s *Server) handleRingState(_ mqttclient.Client, msg mqttclient.Message) {
	topic := msg.Topic()
	s.ringMu.Lock()
	bindings := append([]ringBinding(nil), s.ringBindings[topic]...)
	s.ringMu.Unlock()
	if len(bindings) == 0 {
		return
	}
	payload := msg.Payload()
	for _, b := range bindings {
		val, ok := ringExtract(payload, b.tmpl)
		if !ok || val == "" {
			continue
		}
		switch {
		case strings.EqualFold(val, "ON"):
			val = "on"
		case strings.EqualFold(val, "OFF"):
			val = "off"
		case strings.EqualFold(val, "LOCKED"):
			val = "locked"
		case strings.EqualFold(val, "UNLOCKED"):
			val = "unlocked"
		}
		e, ok := s.store.Get(b.entityID)
		if !ok || e.State == val {
			continue
		}
		e.State = val
		s.store.Set(e)
	}
}

// ringExtract pulls the value for an entity from a ring state payload, applying the
// value_template's JSON field when present, else using the raw payload.
func ringExtract(payload []byte, tmpl string) (string, bool) {
	if strings.TrimSpace(tmpl) == "" {
		return strings.TrimSpace(string(payload)), true
	}
	m := ringValueKey.FindStringSubmatch(tmpl)
	if m == nil {
		return strings.TrimSpace(string(payload)), true
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return "", false
	}
	v, ok := obj[m[1]]
	if !ok || v == nil {
		return "", false
	}
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64), true
	case bool:
		if n {
			return "on", true
		}
		return "off", true
	case string:
		return n, true
	default:
		return fmt.Sprintf("%v", v), true
	}
}

// zwaveLockBySegment returns the configured lock (config: mqtt.zwave_locks) for a zwave-js-ui
// named node segment. We key on the NAMED segment because zwave-js-ui uses the node name in
// both the state and the /set topics once a node is named; the nodeID_<n> topics carry state
// only, not commands.
func (s *Server) zwaveLockBySegment(seg string) (config.ZwaveLock, bool) {
	for _, lk := range s.cfg.ZwaveLocks {
		if lk.Node == seg {
			return lk, true
		}
	}
	return config.ZwaveLock{}, false
}

// handleZwaveState ingests zwave-js-ui Door Lock (CC 98) state and keeps a lock.<id>
// entity in sync. Topics: zwave/nodeID_<n>/98/0/{boltStatus,currentMode}, payload
// {"time":..,"value":..}. Commands go out via handleServiceCallMQTT (source "zwavejs").
func (s *Server) handleZwaveState(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) < 5 || parts[2] != "98" {
		return
	}
	lk, ok := s.zwaveLockBySegment(parts[1])
	if !ok {
		return
	}
	var obj map[string]any
	if json.Unmarshal(msg.Payload(), &obj) != nil {
		return
	}
	state := ""
	switch parts[len(parts)-1] {
	case "boltStatus":
		if sv, _ := obj["value"].(string); sv != "" {
			if ls := strings.ToLower(sv); ls == "locked" || ls == "unlocked" {
				state = ls
			}
		}
	case "currentMode":
		if fv, ok := obj["value"].(float64); ok {
			switch int(fv) {
			case 255:
				state = "locked"
			case 0:
				state = "unlocked"
			}
		}
	default:
		return
	}
	if state == "" {
		return
	}

	id := "lock." + lk.ID
	e, exists := s.store.Get(id)
	src, _ := e.Attributes["source"].(string)
	needMeta := !exists || src != "zwavejs" // seed, or convert a stale ring/other entity
	if needMeta {
		e = entity.Entity{ID: id, Domain: "lock", Attributes: map[string]any{}}
	}
	if e.Attributes == nil {
		e.Attributes = map[string]any{}
	}
	e.Name = lk.Name
	e.Domain = "lock"
	e.Attributes["source"] = "zwavejs"
	e.Attributes["friendly_name"] = lk.Name
	e.Attributes["device"] = lk.ID
	e.Attributes["command_topic"] = "zwave/" + parts[1] + "/98/0/targetMode/set"
	if e.State == state && !needMeta {
		return
	}
	e.State = state
	s.store.Set(e)
}

// handleTasmotaDiscovery parses retained Tasmota auto-discovery messages to seed
// entities. Topic: tasmota/discovery/{mac}/config
func (s *Server) handleTasmotaDiscovery(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) < 4 || parts[len(parts)-1] != "config" {
		return
	}

	var cfg map[string]any
	if err := json.Unmarshal(msg.Payload(), &cfg); err != nil {
		return
	}

	topic, _ := cfg["t"].(string)
	if topic == "" {
		return
	}

	deviceName, _ := cfg["dn"].(string)
	if deviceName == "" {
		deviceName = topic
	}

	rl, _ := cfg["rl"].([]any)
	fn, _ := cfg["fn"].([]any)

	// Count active relays to decide whether to add index suffixes.
	activeRelays := 0
	for _, v := range rl {
		if f, ok := v.(float64); ok && int(f) != 0 {
			activeRelays++
		}
	}

	for i, v := range rl {
		f, ok := v.(float64)
		if !ok {
			continue
		}
		relayType := int(f)
		if relayType == 0 {
			continue
		}

		domain := "switch"
		if relayType == 3 {
			domain = "light"
		}

		name := deviceName
		if i < len(fn) {
			if n, ok := fn[i].(string); ok && n != "" {
				name = n
			}
		}

		entityID := domain + "." + sanitizeID(topic)
		if activeRelays > 1 {
			entityID = fmt.Sprintf("%s.%s_%d", domain, sanitizeID(topic), i+1)
		}

		if existing, exists := s.store.Get(entityID); exists && existing.State != "unknown" {
			continue
		}

		s.store.Set(entity.Entity{
			ID:     entityID,
			Name:   name,
			Domain: domain,
			State:  "unknown",
			Attributes: map[string]any{
				"tasmota_topic": topic,
				"source":        "tasmota",
			},
		})
	}
}

// handleLiquidFWSet handles HA command messages for LiquidFW entities.
// Topic: liquidfw/{entity_id}/set  Payload: {state:on} or {value:128}
func (s *Server) handleLiquidFWSet(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.Split(msg.Topic(), "/")
	slog.Info("liquidfw: mqtt cmd recv", "topic", msg.Topic(), "parts", len(parts), "payload", string(msg.Payload()))
	if len(parts) < 3 {
		return
	}
	entityID := parts[1]
	s.bus.Publish("liquidfw.cmd", map[string]any{
		"entity_id": entityID,
		"raw":       msg.Payload(),
	})
}

// sendNtfy delivers a push notification via ntfy (config: mqtt.ntfy_url). Free, self-hostable;
// the phone subscribes to the topic in the ntfy app. Message/title/priority/tags come from the
// automation action's `data`. No-op if ntfy_url is unset.
func (s *Server) sendNtfy(data map[string]any) {
	url := s.cfg.NtfyURL
	if url == "" {
		return
	}
	msg, _ := data["message"].(string)
	if msg == "" {
		msg = "HomeForge alert"
	}
	req, err := http.NewRequest("POST", url, strings.NewReader(msg))
	if err != nil {
		return
	}
	if v, ok := data["title"].(string); ok && v != "" {
		req.Header.Set("Title", v)
	}
	if v, ok := data["priority"].(string); ok && v != "" {
		req.Header.Set("Priority", v)
	}
	if v, ok := data["tags"].(string); ok && v != "" {
		req.Header.Set("Tags", v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		slog.Warn("notify: ntfy post failed", "err", err)
		return
	}
	resp.Body.Close()
	slog.Info("notify: sent", "title", data["title"], "code", resp.StatusCode)
}

// sendPush fans a notify.* action out to registered app devices via FCM (the push relay).
// The Android channel comes from data.channel, or is derived from data.priority when absent:
// urgent/max -> critical, high -> doorbell, low/min -> motion, else default. data.click (if
// present) rides along as a deep-link target the app opens when the notification is tapped.
func (s *Server) sendPush(data map[string]any) {
	msg, _ := data["message"].(string)
	if msg == "" {
		msg = "HomeForge alert"
	}
	title, _ := data["title"].(string)
	if title == "" {
		title = "HomeForge"
	}
	channel, _ := data["channel"].(string)
	if channel == "" {
		switch p, _ := data["priority"].(string); p {
		case "urgent", "max":
			channel = "critical"
		case "high":
			channel = "doorbell"
		case "low", "min":
			channel = "motion"
		default:
			channel = "default"
		}
	}
	tag, _ := data["tag"].(string)
	if tag == "" { // collapse repeats of the same event type by default
		tag = channel
	}
	var pdata map[string]string
	if click, ok := data["click"].(string); ok && click != "" {
		pdata = map[string]string{"click": click}
	}
	image, _ := data["image"].(string)
	push.Send(s.cfg.PushRelayURL, push.Payload{
		Title: title, Message: msg, Image: image, Channel: channel, Tag: tag, Data: pdata,
	})
}

// handleServiceCallMQTT publishes MQTT commands for Zigbee2MQTT and Tasmota entities.
func (s *Server) handleServiceCallMQTT(entityID, service string, data map[string]any) {
	// Push notifications — handled before the entity lookup (no entity needed). Fans out to both
	// the ntfy topic (if configured) and the in-app FCM relay (registered devices).
	if strings.HasPrefix(service, "notify.") {
		s.sendNtfy(data)
		s.sendPush(data)
		return
	}
	if s.client == nil {
		return
	}

	e, ok := s.store.Get(entityID)
	if !ok {
		return
	}

	if _, isWiz := e.Attributes["wiz_mac"]; isWiz {
		return // WiZ handled by the wiz integration directly, not via MQTT
	}
	if _, isWled := e.Attributes["wled_host"]; isWled {
		return // WLED handled by the wled integration directly
	}

	// zwave-js-ui door locks: set the Door Lock target mode (255 = lock, 0 = unlock) by
	// publishing {"value":N} to the value's /set topic.
	if src, _ := e.Attributes["source"].(string); src == "zwavejs" {
		ct, _ := e.Attributes["command_topic"].(string)
		if ct == "" {
			return
		}
		lc := strings.ToLower(service)
		val := -1
		switch {
		case strings.HasSuffix(lc, ".unlock"):
			val = 0
		case strings.HasSuffix(lc, ".lock"):
			val = 255
		}
		if val < 0 {
			return
		}
		s.Publish(ct, []byte(fmt.Sprintf(`{"value":%d}`, val)))
		slog.Info("zwave: lock command", "entity", entityID, "value", val)
		return
	}

	// Ring devices: publish HA-standard payloads to the entity's command_topic (ring-mqtt sidecar).
	if src, _ := e.Attributes["source"].(string); src == "ring" {
		ct, _ := e.Attributes["command_topic"].(string)
		if ct == "" {
			return
		}
		lc := strings.ToLower(service)
		payload := ""
		switch {
		case strings.HasSuffix(lc, ".alarm_disarm"):
			payload = "DISARM"
		case strings.HasSuffix(lc, ".alarm_arm_home"):
			payload = "ARM_HOME"
		case strings.HasSuffix(lc, ".alarm_arm_away"):
			payload = "ARM_AWAY"
		case strings.HasSuffix(lc, ".alarm_arm_night"):
			payload = "ARM_NIGHT"
		case strings.HasSuffix(lc, ".lock"):
			payload = "LOCK"
		case strings.HasSuffix(lc, ".unlock"):
			payload = "UNLOCK"
		case strings.HasSuffix(lc, ".turn_on"):
			payload = "ON"
		case strings.HasSuffix(lc, ".turn_off"):
			payload = "OFF"
		case strings.HasSuffix(lc, ".toggle"):
			if e.State == "on" {
				payload = "OFF"
			} else {
				payload = "ON"
			}
		default:
			return
		}
		s.Publish(ct, []byte(payload))
		slog.Info("ring: command", "entity", entityID, "payload", payload)
		return
	}

	// VeSync devices: publish a command to vesync/<dev>/set for the vesync-bridge sidecar.
	if dev, isVesync := e.Attributes["vesync_dev"].(string); isVesync {
		cmd, _ := e.Attributes["vesync_cmd"].(string)
		lc := strings.ToLower(service)
		topic := fmt.Sprintf("vesync/%s/set", dev)
		if strings.HasSuffix(lc, ".set_value") { // number: fan / humidity
			if val, ok := data["value"]; ok {
				payload, _ := json.Marshal(map[string]any{cmd: val})
				s.Publish(topic, payload)
			}
			return
		}
		var on bool // switch: power
		switch {
		case strings.HasSuffix(lc, ".turn_on"):
			on = true
		case strings.HasSuffix(lc, ".turn_off"):
			on = false
		case strings.HasSuffix(lc, ".toggle"):
			on = e.State != "on"
		default:
			return
		}
		st := "off"
		if on {
			st = "on"
		}
		payload, _ := json.Marshal(map[string]any{"power": st})
		s.Publish(topic, payload)
		return
	}

	// Hubspace devices: publish a command to hubspace/<dev>/set for the hubspace-bridge sidecar.
	if dev, isHub := e.Attributes["hubspace_dev"].(string); isHub {
		cmd, _ := e.Attributes["hubspace_cmd"].(string)
		lc := strings.ToLower(service)
		topic := fmt.Sprintf("hubspace/%s/set", dev)
		if strings.HasSuffix(lc, ".set_value") { // number: brightness / speed
			if val, ok := data["value"]; ok {
				payload, _ := json.Marshal(map[string]any{cmd: val})
				s.Publish(topic, payload)
			}
			return
		}
		var on bool // switch: power
		switch {
		case strings.HasSuffix(lc, ".turn_on"):
			on = true
		case strings.HasSuffix(lc, ".turn_off"):
			on = false
		case strings.HasSuffix(lc, ".toggle"):
			on = e.State != "on"
		default:
			return
		}
		st := "off"
		if on {
			st = "on"
		}
		payload, _ := json.Marshal(map[string]any{"power": st})
		s.Publish(topic, payload)
		return
	}

	lc := strings.ToLower(service)

	// number.set_value — a numeric value (e.g. iFan04 speed 0-3) carried in data["value"].
	// Only LiquidFW number entities are wired for this today.
	if strings.HasSuffix(lc, ".set_value") {
		val, ok := data["value"]
		if !ok {
			return
		}
		if _, ok := e.Attributes["device"].(string); ok {
			payload, _ := json.Marshal(map[string]any{"value": val})
			s.Publish(fmt.Sprintf("liquidfw/%s/set", entityID), payload)
		}
		return
	}

	var on bool
	switch {
	case strings.HasSuffix(lc, ".turn_on"):
		on = true
	case strings.HasSuffix(lc, ".turn_off"):
		on = false
	case strings.HasSuffix(lc, ".toggle"):
		on = e.State != "on"
	default:
		return
	}

	state := "OFF"
	if on {
		state = "ON"
	}

	// Tasmota entities carry tasmota_topic in their attributes.
	if t, ok := e.Attributes["tasmota_topic"].(string); ok {
		s.Publish(fmt.Sprintf("cmnd/%s/POWER", t), []byte(state))
		return
	}

	// Zigbee2MQTT entities carry z2m_topic in their attributes.
	if t, ok := e.Attributes["z2m_topic"].(string); ok {
		payload, _ := json.Marshal(map[string]any{"state": state})
		s.Publish(fmt.Sprintf("zigbee2mqtt/%s/set", t), payload)
		return
	}

	// LiquidFW entities carry "device" attribute.
	if _, ok := e.Attributes["device"].(string); ok {
		payload, _ := json.Marshal(map[string]any{"state": state})
		s.Publish(fmt.Sprintf("liquidfw/%s/set", entityID), payload)
		return
	}

	// Sonoff entities carry "sonoff_device" attribute.
	if _, ok := e.Attributes["sonoff_device"].(string); ok {
		s.bus.Publish("sonoff.cmd", map[string]any{
			"entity_id": entityID,
			"state":     state,
		})
		return
	}
}

// Publish sends a message to the MQTT broker.
func (s *Server) Publish(topic string, payload []byte) {
	if s.client != nil {
		s.client.Publish(topic, 0, false, payload)
	}
}

func inferDomainState(payload map[string]any) (domain, state string) {
	if _, ok := payload["occupancy"]; ok {
		if occ, _ := payload["occupancy"].(bool); occ {
			return "binary_sensor", "on"
		}
		return "binary_sensor", "off"
	}
	if _, ok := payload["contact"]; ok {
		if contact, _ := payload["contact"].(bool); contact {
			return "binary_sensor", "on"
		}
		return "binary_sensor", "off"
	}
	if state, ok := payload["state"].(string); ok {
		if state == "ON" || state == "OFF" {
			return "light", strings.ToLower(state)
		}
	}
	if _, ok := payload["temperature"]; ok {
		if temp, ok := payload["temperature"].(float64); ok {
			return "sensor", fmt.Sprintf("%.1f", temp)
		}
	}
	return "sensor", "unknown"
}

func sanitizeID(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return '_'
	}, s)
}
