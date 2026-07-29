package liquidfw

import (
	"bytes"
	"context"
	"encoding/base64"
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

var cmdHTTP = &http.Client{Timeout: 5 * time.Second}

// Manager is the LiquidFW integration entry point.
type Manager struct {
	cfg   config.LiquidFWConfig
	store *entity.Store
	bus   *bus.Bus
	reg   *registry // live registry (set in Run); used by SendToDevice

	// lastBool tracks the last PHYSICAL value we published for each bool pin entity, so a
	// device heartbeat re-broadcasting an UNCHANGED pin doesn't clobber a value an automation
	// set on that entity. This lets a dumb capacitive touchpad's entity be driven to follow
	// the light (light→touchpad sync) — the device only owns it on a real physical change.
	lastBoolMu sync.Mutex
	lastBool   map[string]bool
}

func NewManager(cfg config.LiquidFWConfig, store *entity.Store, b *bus.Bus) *Manager {
	return &Manager{cfg: cfg, store: store, bus: b, lastBool: map[string]bool{}}
}

// Run starts the UDP listener and blocks until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("liquidfw: disabled")
		<-ctx.Done()
		return
	}

	priv, err := loadOrGenKey(m.cfg.KeyFile)
	if err != nil {
		slog.Error("liquidfw: load key", "err", err)
		<-ctx.Done()
		return
	}
	slog.Info("liquidfw: starting",
		"pub", base64.StdEncoding.EncodeToString(pubKeyBytes(priv)),
		"udp_port", m.cfg.UDPPort)

	reg := newRegistry(m.cfg.RegistryFile, priv)
	m.reg = reg // expose the live registry to SendToDevice (thermostat brain, etc.)

	// Subscribe to MQTT command messages forwarded by the MQTT server.
	// HA sends: liquidfw/switch.liquidfw_test_led/set {"state":"on"}
	m.bus.Subscribe("liquidfw.cmd", func(ev bus.Event) {
		payload, ok := ev.Payload.(map[string]any)
		if !ok {
			return
		}
		entityID, _ := payload["entity_id"].(string)
		raw, _ := payload["raw"].([]byte)
		if entityID == "" || len(raw) == 0 {
			return
		}
		if err := m.handleCmd(reg, entityID, raw); err != nil {
			slog.Warn("liquidfw: cmd failed", "entity", entityID, "err", err)
		}
	})

	// HTTP fast-poll goroutines for devices that need sub-broadcast-interval latency.
	for _, p := range m.cfg.HTTPPoll {
		go m.runHTTPPoll(p.IP, p.Name, p.IntervalMs)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- listenUDP(m.cfg.UDPPort, reg, func(deviceID uint32, s statePacket, srcIP string) {
			m.publishEntities(deviceID, s)
		})
	}()

	select {
	case err := <-errCh:
		slog.Error("liquidfw: udp listener stopped", "err", err)
	case <-ctx.Done():
	}
}

// Pubkey prints the homeforge public key for the given key file (CLI helper).
func Pubkey(keyFile string) (string, error) {
	priv, err := loadOrGenKey(keyFile)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pubKeyBytes(priv)), nil
}

// Provision POSTs homeforge_pub to a device's /provision endpoint.
func Provision(keyFile, deviceIP string) error {
	priv, err := loadOrGenKey(keyFile)
	if err != nil {
		return err
	}
	pub := base64.StdEncoding.EncodeToString(pubKeyBytes(priv))
	body := fmt.Sprintf(`{"homeforge_pub":"%s"}`, pub)
	resp, err := cmdHTTP.Post("http://"+deviceIP+"/provision", "application/json", strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("provision request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("device returned %d: %s", resp.StatusCode, b)
	}
	return nil
}

// SendCmd sends a signed command to a named device.
func SendCmd(keyFile, registryFile, deviceName string, payload []byte) error {
	priv, err := loadOrGenKey(keyFile)
	if err != nil {
		return err
	}
	reg := newRegistry(registryFile, priv)
	d := reg.findByName(deviceName)
	if d == nil {
		return fmt.Errorf("unknown device: %s", deviceName)
	}
	return sendCmd(d, reg, payload)
}

func sendCmd(d *device, reg *registry, body []byte) error {
	if d.IP == "" {
		return fmt.Errorf("no IP for device %08x", d.ID)
	}
	nonce := reg.nextCmdNonce(d.ID)
	nonceHex, sigHex := buildCmdAuth(d.SessionKey, nonce, body)

	req, err := http.NewRequest("POST", "http://"+d.IP+"/cmd", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nonce", nonceHex)
	req.Header.Set("X-Sig", sigHex)

	resp, err := cmdHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("device %d: %s", resp.StatusCode, b)
	}
	return nil
}

// SendToDevice sends a signed raw command (arbitrary key/value pairs) to a device
// by name, using the LIVE registry so the current session key is always used. Used
// by the thermostat brain to push mode/setpoint/temp. Returns an error if the
// integration hasn't started, or the device is unknown / not yet keyed.
func (m *Manager) SendToDevice(name string, body map[string]any) error {
	if m.reg == nil {
		return fmt.Errorf("liquidfw not started")
	}
	d := m.reg.findByName(name)
	if d == nil {
		return fmt.Errorf("unknown device: %s", name)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return sendCmd(d, m.reg, raw)
}

// handleCmd processes an inbound MQTT command for a LiquidFW entity.
// HA sends: liquidfw/switch.liquidfw_test_led/set {"state":"on"}
// Entity attrs carry "device" (device name) and "pin_name" (original pin key).
func (m *Manager) handleCmd(reg *registry, entityID string, raw []byte) error {
	e, ok := m.store.Get(entityID)
	if !ok {
		return fmt.Errorf("unknown entity: %s", entityID)
	}

	deviceName, _ := e.Attributes["device"].(string)
	pinName, _ := e.Attributes["pin_name"].(string)
	if deviceName == "" || pinName == "" {
		return fmt.Errorf("entity %s missing device/pin_name attrs", entityID)
	}

	d := reg.findByName(deviceName)
	if d == nil {
		return fmt.Errorf("unknown device: %s", deviceName)
	}

	domain := strings.SplitN(entityID, ".", 2)[0]

	var cmdBody map[string]any
	switch domain {
	case "switch", "light":
		var p struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("bad payload: %w", err)
		}
		cmdBody = map[string]any{pinName: strings.ToLower(p.State) == "on"}

	case "number":
		var p struct {
			Value float64 `json:"value"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("bad payload: %w", err)
		}
		cmdBody = map[string]any{pinName: p.Value}

	default:
		return fmt.Errorf("unsupported domain for cmd: %s", domain)
	}

	body, err := json.Marshal(cmdBody)
	if err != nil {
		return err
	}
	slog.Info("liquidfw: cmd", "device", deviceName, "pin", pinName, "body", string(body))
	// sendCmd may fail because the target pin is an INPUT the device won't drive (a dumb
	// touchpad). That's expected — we still update the HF entity below so automations can own
	// its display state (e.g. a touchpad entity driven to follow the light). For a real output
	// the device accepts the command and drives the hardware.
	if err := sendCmd(d, reg, body); err != nil {
		slog.Debug("liquidfw: device did not accept cmd (input pin?)", "entity", entityID, "err", err)
	}
	// State update (regardless of device acceptance) — also seeds lastBool so a following
	// device heartbeat with the same raw value won't clobber this automation-set state.
	if v, ok := cmdBody[pinName].(bool); ok {
		newState := "off"
		if v {
			newState = "on"
		}
		if e, ok2 := m.store.Get(entityID); ok2 {
			e.State = newState
			m.store.Set(e)
		}
	}
	return nil
}

// runHTTPPoll polls a LiquidFW device's GET /state endpoint at a fast interval
// so automations react in <interval_ms instead of waiting for the 5s UDP broadcast.
func (m *Manager) runHTTPPoll(ip, name string, intervalMs int) {
	if intervalMs <= 0 {
		intervalMs = 500
	}
	var lastState string
	for {
		time.Sleep(time.Duration(intervalMs) * time.Millisecond)
		resp, err := cmdHTTP.Get("http://" + ip + "/state")
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var s statePacket
		if err := json.Unmarshal(body, &s); err != nil {
			continue
		}
		if s.Name == "" {
			s.Name = name
		}
		// Only publish when IO state actually changes to avoid flooding the bus.
		ioStr := fmt.Sprintf("%v", s.IO)
		if ioStr == lastState {
			continue
		}
		lastState = ioStr
		m.publishEntities(0, s)
	}
}

// publishEntities converts a device state packet into homeforge entities.
func (m *Manager) publishEntities(deviceID uint32, s statePacket) {
	name := s.Name
	if name == "" {
		name = fmt.Sprintf("device_%08x", deviceID)
	}
	slug := strings.ReplaceAll(strings.ToLower(name), "-", "_")

	for pinName, rawVal := range s.IO {
		pinSlug := strings.ReplaceAll(strings.ToLower(pinName), "-", "_")

		switch v := rawVal.(type) {
		case bool:
			state := "off"
			if v {
				state = "on"
			}
			id := fmt.Sprintf("switch.%s_%s", slug, pinSlug)
			// Only (re)publish this bool pin when its PHYSICAL value actually changed since
			// the last broadcast. Skipping unchanged heartbeats means an automation-set value
			// (e.g. a touchpad entity driven to follow the light) is NOT clobbered back to the
			// raw pad position every 5s. A real touch flips the raw value → we publish it, so
			// touch control still fires normally.
			m.lastBoolMu.Lock()
			prev, seen := m.lastBool[id]
			unchanged := seen && prev == v
			m.lastBool[id] = v
			m.lastBoolMu.Unlock()
			if unchanged {
				break
			}
			m.store.Set(entity.Entity{
				ID:     id,
				Name:   fmt.Sprintf("%s %s", name, pinName),
				Domain: "switch",
				State:  state,
				Attributes: map[string]any{
					"device":   name,
					"pin_name": pinName,
					"pin_type": "relay",
					"rssi":     s.RSSI,
					"uptime":   s.Uptime,
				},
			})

		case float64:
			var domain, state, pinType string
			if v == float64(int64(v)) {
				domain = "number"
				state = fmt.Sprintf("%d", int64(v))
				pinType = "pwm"
			} else {
				domain = "sensor"
				state = fmt.Sprintf("%.2f", v)
				pinType = "analog"
			}
			m.store.Set(entity.Entity{
				ID:     fmt.Sprintf("%s.%s_%s", domain, slug, pinSlug),
				Name:   fmt.Sprintf("%s %s", name, pinName),
				Domain: domain,
				State:  state,
				Attributes: map[string]any{
					"device":   name,
					"pin_name": pinName,
					"pin_type": pinType,
					"rssi":     s.RSSI,
					"uptime":   s.Uptime,
				},
			})

		case map[string]any:
			// A nested value = a multi-reading sensor (dht22 temp/humidity, sgp30
			// eco2/tvoc, bmp280 temp/pressure, …). Expose one read-only sensor per key.
			units := map[string]string{
				"temp": "°C", "temperature": "°C", "humidity": "%",
				"pressure": "hPa", "eco2": "ppm", "tvoc": "ppb",
			}
			labels := map[string]string{
				"temp": "Temperature", "temperature": "Temperature", "humidity": "Humidity",
				"pressure": "Pressure", "eco2": "eCO2", "tvoc": "TVOC",
			}
			for key, val := range v {
				suffix := key
				if key == "temp" {
					suffix = "temperature"
				}
				label := labels[key]
				if label == "" {
					label = key
				}
				m.store.Set(entity.Entity{
					ID:     fmt.Sprintf("sensor.%s_%s_%s", slug, pinSlug, suffix),
					Name:   fmt.Sprintf("%s %s %s", name, pinName, label),
					Domain: "sensor",
					State:  fmt.Sprintf("%v", val),
					Attributes: map[string]any{
						"device":              name,
						"pin_name":            pinName,
						"unit_of_measurement": units[key],
						"rssi":                s.RSSI,
						"uptime":              s.Uptime,
					},
				})
			}

		default:
			m.store.Set(entity.Entity{
				ID:     fmt.Sprintf("sensor.%s_%s", slug, pinSlug),
				Name:   fmt.Sprintf("%s %s", name, pinName),
				Domain: "sensor",
				State:  fmt.Sprintf("%v", rawVal),
				Attributes: map[string]any{
					"device": name,
					"rssi":   s.RSSI,
					"uptime": s.Uptime,
				},
			})
		}
	}

	// Device-level uptime (no "device" attr → filtered out of MQTT)
	m.store.Set(entity.Entity{
		ID:     fmt.Sprintf("sensor.%s_uptime", slug),
		Name:   fmt.Sprintf("%s Uptime", name),
		Domain: "sensor",
		State:  fmt.Sprintf("%d", s.Uptime),
		Attributes: map[string]any{
			"unit_of_measurement": "s",
			"rssi":                s.RSSI,
		},
	})
}
