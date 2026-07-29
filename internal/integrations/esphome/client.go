package esphome

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

// ESPHome native API message type IDs.
const (
	msgHelloRequest             = 1
	msgHelloResponse            = 2
	msgDisconnectRequest        = 3
	msgPingRequest              = 5
	msgPingResponse             = 6
	msgConnectRequest           = 7
	msgConnectResponse          = 8
	msgListEntitiesRequest      = 11
	msgListEntitiesBinarySensor = 12
	msgListEntitiesLight        = 15
	msgListEntitiesSensor       = 16
	msgListEntitiesSwitch       = 17
	msgListEntitiesDone         = 19
	msgSubscribeStatesRequest   = 20
	msgBinarySensorState        = 21
	msgLightState               = 24
	msgSensorState              = 25
	msgSwitchState              = 26
	msgLightCommand             = 32
	msgSwitchCommand            = 33
	msgGetTimeRequest           = 36
	msgGetTimeResponse          = 37
)

type entityMeta struct {
	key    uint32
	domain string
	id     string
	name   string
	unit   string
}

// Client manages a persistent connection to one ESPHome device.
type Client struct {
	cfg      config.ESPHomeDevice
	store    *entity.Store
	bus      *bus.Bus
	deviceID string

	mu   sync.Mutex
	keys map[uint32]entityMeta
	ids  map[string]uint32
	api  msgConn // nil when disconnected
}

func NewClient(cfg config.ESPHomeDevice, store *entity.Store, b *bus.Bus) *Client {
	name := cfg.Name
	if name == "" {
		name = cfg.Host
	}
	return &Client{
		cfg:      cfg,
		store:    store,
		bus:      b,
		deviceID: sanitize(name),
		keys:     make(map[uint32]entityMeta),
		ids:      make(map[string]uint32),
	}
}

func (c *Client) Run(ctx context.Context) {
	c.bus.Subscribe("service.call", func(ev bus.Event) {
		payload, ok := ev.Payload.(map[string]any)
		if !ok {
			return
		}
		entityID, _ := payload["entity"].(string)
		service, _ := payload["service"].(string)
		c.handleServiceCall(entityID, service)
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := c.connect(ctx); err != nil {
			slog.Warn("esphome: disconnected", "device", c.cfg.Name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func (c *Client) connect(ctx context.Context) error {
	port := c.cfg.Port
	if port == 0 {
		port = 6053
	}
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, port)

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Negotiate transport: noise if PSK is configured, otherwise plaintext.
	var api msgConn
	if c.cfg.NoisePSK != "" {
		nc, err := establishNoise(conn, c.cfg.NoisePSK)
		if err != nil {
			return fmt.Errorf("noise: %w", err)
		}
		api = nc
	} else {
		api = newPlaintextConn(conn)
	}

	c.mu.Lock()
	c.api = api
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.api = nil
		c.mu.Unlock()
	}()

	// Hello handshake.
	hello := protoString(3, "HomeForge")
	hello = append(hello, protoVarint(4, 1)...)  // api_version_major
	hello = append(hello, protoVarint(5, 10)...) // api_version_minor
	if err := api.writeMsg(msgHelloRequest, hello); err != nil {
		return err
	}
	if _, _, err := api.readMsg(); err != nil {
		return fmt.Errorf("hello response: %w", err)
	}

	// Connect (optional password for plaintext mode).
	var connReq []byte
	if c.cfg.Password != "" {
		connReq = protoString(1, c.cfg.Password)
	}
	if err := api.writeMsg(msgConnectRequest, connReq); err != nil {
		return err
	}
	_, connData, err := api.readMsg()
	if err != nil {
		return fmt.Errorf("connect response: %w", err)
	}
	if fields, _ := decodeProto(connData); len(fields) > 0 {
		for _, f := range fields {
			if f.num == 1 {
				if v, _ := f.val.(uint64); v != 0 {
					return fmt.Errorf("device rejected connection (wrong password?)")
				}
			}
		}
	}

	// List entities.
	if err := api.writeMsg(msgListEntitiesRequest, nil); err != nil {
		return err
	}

	c.mu.Lock()
	c.keys = make(map[uint32]entityMeta)
	c.ids = make(map[string]uint32)
	c.mu.Unlock()

	for {
		msgType, data, err := api.readMsg()
		if err != nil {
			return fmt.Errorf("list entities: %w", err)
		}
		if msgType == msgListEntitiesDone {
			break
		}
		c.handleListEntity(msgType, data)
	}

	slog.Info("esphome: connected", "device", c.cfg.Name, "addr", addr)

	if err := api.writeMsg(msgSubscribeStatesRequest, nil); err != nil {
		return err
	}

	type frame struct {
		t int
		d []byte
	}
	frameCh := make(chan frame, 32)
	errCh := make(chan error, 1)

	go func() {
		for {
			msgType, data, err := api.readMsg()
			if err != nil {
				errCh <- err
				return
			}
			frameCh <- frame{msgType, data}
		}
	}()

	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = api.writeMsg(msgDisconnectRequest, nil)
			return nil
		case err := <-errCh:
			return err
		case f := <-frameCh:
			switch f.t {
			case msgPingRequest:
				_ = api.writeMsg(msgPingResponse, nil)
			case msgGetTimeRequest:
				resp := protoVarint(1, uint64(time.Now().Unix()))
				_ = api.writeMsg(msgGetTimeResponse, resp)
			default:
				c.handleState(f.t, f.d)
			}
		case <-pingTicker.C:
			_ = api.writeMsg(msgPingRequest, nil)
		}
	}
}

func (c *Client) handleListEntity(msgType int, data []byte) {
	fields, err := decodeProto(data)
	if err != nil {
		return
	}

	var objectID, name, unit string
	var key uint32
	for _, f := range fields {
		switch {
		case f.num == 1 && f.wire == 2:
			objectID = string(f.val.([]byte))
		case f.num == 2 && f.wire == 5:
			key = f.val.(uint32)
		case f.num == 3 && f.wire == 2:
			name = string(f.val.([]byte))
		case f.num == 6 && f.wire == 2: // unit_of_measurement (sensor)
			unit = string(f.val.([]byte))
		}
	}

	if key == 0 {
		return
	}

	var domain string
	switch msgType {
	case msgListEntitiesBinarySensor:
		domain = "binary_sensor"
	case msgListEntitiesLight:
		domain = "light"
	case msgListEntitiesSensor:
		domain = "sensor"
	case msgListEntitiesSwitch:
		domain = "switch"
	default:
		return
	}

	if name == "" {
		name = objectID
	}
	entityID := domain + ".esphome_" + c.deviceID + "_" + sanitize(objectID)

	meta := entityMeta{key: key, domain: domain, id: entityID, name: name, unit: unit}
	c.mu.Lock()
	c.keys[key] = meta
	c.ids[entityID] = key
	c.mu.Unlock()

	attrs := map[string]any{"source": "esphome", "device": c.cfg.Name}
	if unit != "" {
		attrs["unit_of_measurement"] = unit
	}

	c.store.Set(entity.Entity{
		ID:         entityID,
		Name:       name,
		Domain:     domain,
		State:      "unknown",
		Attributes: attrs,
	})
}

func (c *Client) handleState(msgType int, data []byte) {
	fields, err := decodeProto(data)
	if err != nil {
		return
	}

	var key uint32
	var state string
	missing := false

	for _, f := range fields {
		switch f.num {
		case 1:
			if v, ok := f.val.(uint32); ok {
				key = v
			}
		case 2:
			switch msgType {
			case msgBinarySensorState, msgLightState, msgSwitchState:
				if v, ok := f.val.(uint64); ok {
					if v != 0 {
						state = "on"
					} else {
						state = "off"
					}
				}
			case msgSensorState:
				if v, ok := f.val.(uint32); ok {
					f32 := math.Float32frombits(v)
					state = fmt.Sprintf("%g", f32)
				}
			}
		case 3:
			if v, ok := f.val.(uint64); ok && v != 0 {
				missing = true
			}
		}
	}

	if key == 0 || state == "" {
		return
	}
	if missing {
		state = "unavailable"
	}

	c.mu.Lock()
	meta, ok := c.keys[key]
	c.mu.Unlock()
	if !ok {
		return
	}

	attrs := map[string]any{"source": "esphome", "device": c.cfg.Name}
	if meta.unit != "" {
		attrs["unit_of_measurement"] = meta.unit
	}

	c.store.Set(entity.Entity{
		ID:         meta.id,
		Name:       meta.name,
		Domain:     meta.domain,
		State:      state,
		Attributes: attrs,
	})
}

func (c *Client) handleServiceCall(entityID, service string) {
	c.mu.Lock()
	key, ok := c.ids[entityID]
	var meta entityMeta
	if ok {
		meta = c.keys[key]
	}
	api := c.api
	c.mu.Unlock()

	if !ok || api == nil {
		return
	}

	var turnOn bool
	lc := strings.ToLower(service)
	switch {
	case strings.HasSuffix(lc, ".turn_on"):
		turnOn = true
	case strings.HasSuffix(lc, ".turn_off"):
		turnOn = false
	case strings.HasSuffix(lc, ".toggle"):
		if e, exists := c.store.Get(entityID); exists {
			turnOn = e.State != "on"
		}
	default:
		return
	}

	var onVal uint64
	if turnOn {
		onVal = 1
	}

	switch meta.domain {
	case "light":
		payload := protoFixed32(1, key)
		payload = append(payload, protoVarint(2, 1)...) // has_state=true
		payload = append(payload, protoVarint(3, onVal)...)
		_ = api.writeMsg(msgLightCommand, payload)
	case "switch":
		payload := protoFixed32(1, key)
		payload = append(payload, protoVarint(2, onVal)...)
		_ = api.writeMsg(msgSwitchCommand, payload)
	}
}

func sanitize(s string) string {
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
