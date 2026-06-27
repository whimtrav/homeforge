package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	mqttclient "github.com/eclipse/paho.mqtt.golang"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

const TopicMQTTMessage = "mqtt.message"

type Message struct {
	Topic   string
	Payload []byte
}

type Server struct {
	cfg     config.MQTTConfig
	store   *entity.Store
	bus     *bus.Bus
	broker  *mqttserver.Server
	client  mqttclient.Client
}

func NewServer(cfg config.MQTTConfig, store *entity.Store, b *bus.Bus) (*Server, error) {
	s := &Server{cfg: cfg, store: store, bus: b}

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
	c.Subscribe("sentinel/+/detection/+", 0, s.handleSentinel)

	// Forward all messages to automation engine.
	c.Subscribe("#", 0, func(_ mqttclient.Client, msg mqttclient.Message) {
		s.bus.Publish(TopicMQTTMessage, Message{
			Topic:   msg.Topic(),
			Payload: msg.Payload(),
		})
	})

	// Request Z2M to re-publish all device states.
	c.Publish("zigbee2mqtt/bridge/request/devices", 0, false, "{}")
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
	switch domain {
	case "sensor", "binary_sensor", "light", "switch", "lock", "climate", "cover":
	default:
		return
	}

	var cfg map[string]any
	if err := json.Unmarshal(msg.Payload(), &cfg); err != nil {
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
		s.store.Set(entity.Entity{
			ID:         id,
			Name:       deviceName,
			Domain:     "switch",
			State:      strings.ToLower(power),
			Attributes: payload,
		})
	}
}

func (s *Server) handleSentinel(_ mqttclient.Client, msg mqttclient.Message) {
	// sentinel/{camera}/detection/{label}
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) < 4 {
		return
	}
	camera := parts[1]
	label := parts[3]
	id := fmt.Sprintf("binary_sensor.sentinel_%s_%s", sanitizeID(camera), sanitizeID(label))

	var payload map[string]any
	_ = json.Unmarshal(msg.Payload(), &payload)
	if payload == nil {
		payload = make(map[string]any)
	}

	state := "on"
	if v, ok := payload["active"].(bool); ok && !v {
		state = "off"
	}

	s.store.Set(entity.Entity{
		ID:         id,
		Name:       fmt.Sprintf("%s %s detected", camera, label),
		Domain:     "binary_sensor",
		State:      state,
		Attributes: payload,
	})
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
