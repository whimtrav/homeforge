package sonoff

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

var httpClient = &http.Client{Timeout: 5 * time.Second}

type Manager struct {
	cfg   config.SonoffConfig
	store *entity.Store
	bus   *bus.Bus
}

func NewManager(cfg config.SonoffConfig, store *entity.Store, b *bus.Bus) *Manager {
	return &Manager{cfg: cfg, store: store, bus: b}
}

func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		<-ctx.Done()
		return
	}

	slog.Info("sonoff: starting", "devices", len(m.cfg.Devices))

	for _, dev := range m.cfg.Devices {
		for _, ent := range dev.Entities {
			m.store.Set(entity.Entity{
				ID:     ent.ID,
				Name:   ent.Name,
				Domain: "switch",
				State:  "off",
				Attributes: map[string]any{
					"sonoff_device": dev.DeviceID,
					"sonoff_ip":     dev.IP,
				},
			})
			slog.Info("sonoff: registered entity", "id", ent.ID, "device", dev.DeviceID, "ip", dev.IP)
		}
	}

	// Seed initial state immediately, then poll every 30s.
	m.pollAll()
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				m.pollAll()
			}
		}
	}()

	m.bus.Subscribe("sonoff.cmd", func(ev bus.Event) {
		payload, ok := ev.Payload.(map[string]any)
		if !ok {
			return
		}
		entityID, _ := payload["entity_id"].(string)
		state, _ := payload["state"].(string)

		for _, dev := range m.cfg.Devices {
			for _, ent := range dev.Entities {
				if ent.ID != entityID {
					continue
				}
				on := state == "on" || state == "ON"
				if err := sendCommand(dev.IP, dev.DeviceID, dev.APIKey, on); err != nil {
					slog.Error("sonoff: cmd failed", "entity", entityID, "err", err)
					return
				}
				newState := "off"
				if on {
					newState = "on"
				}
				m.store.Set(entity.Entity{
					ID:     entityID,
					Name:   ent.Name,
					Domain: "switch",
					State:  newState,
					Attributes: map[string]any{
						"sonoff_device": dev.DeviceID,
						"sonoff_ip":     dev.IP,
					},
				})
				slog.Info("sonoff: cmd sent", "entity", entityID, "state", newState)
				return
			}
		}
		slog.Warn("sonoff: no device for entity", "entity", entityID)
	})

	<-ctx.Done()
}

func (m *Manager) pollAll() {
	for _, dev := range m.cfg.Devices {
		sw, err := queryState(dev.IP, dev.DeviceID, dev.APIKey)
		if err != nil {
			// 404 = device doesn't support /zeroconf/info (e.g. Sonoff MINI); log at debug
		if strings.Contains(err.Error(), "error 404") {
			slog.Debug("sonoff: poll not supported", "device", dev.DeviceID, "err", err)
		} else {
			slog.Warn("sonoff: poll failed", "device", dev.DeviceID, "ip", dev.IP, "err", err)
		}
			continue
		}
		for _, ent := range dev.Entities {
			m.store.Set(entity.Entity{
				ID:     ent.ID,
				Name:   ent.Name,
				Domain: "switch",
				State:  sw,
				Attributes: map[string]any{
					"sonoff_device": dev.DeviceID,
					"sonoff_ip":     dev.IP,
				},
			})
		}
		slog.Debug("sonoff: polled", "device", dev.DeviceID, "switch", sw)
	}
}

// queryState calls /zeroconf/info and returns the current switch state ("on"/"off").
func queryState(ip, deviceID, apiKey string) (string, error) {
	encrypted, iv, err := encryptParams(apiKey, []byte("{}"))
	if err != nil {
		return "", err
	}

	body, _ := json.Marshal(map[string]any{
		"sequence":   fmt.Sprintf("%d", time.Now().UnixMilli()),
		"deviceid":   deviceID,
		"selfApikey": "###",
		"iv":         base64.StdEncoding.EncodeToString(iv),
		"encrypt":    true,
		"data":       base64.StdEncoding.EncodeToString(encrypted),
	})

	url := fmt.Sprintf("http://%s:8081/zeroconf/info", ip)
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("sonoff: info http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("sonoff: info HTTP %d", resp.StatusCode)
	}

	raw, _ := io.ReadAll(resp.Body)
	var reply struct {
		Error int    `json:"error"`
		Data  string `json:"data"`
		IV    string `json:"iv"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("sonoff: info parse: %w", err)
	}
	if reply.Error != 0 {
		return "", fmt.Errorf("sonoff: info error %d", reply.Error)
	}

	plain, err := decryptData(apiKey, reply.IV, reply.Data)
	if err != nil {
		return "", fmt.Errorf("sonoff: info decrypt: %w", err)
	}

	var state struct {
		Switch string `json:"switch"`
	}
	if err := json.Unmarshal(plain, &state); err != nil {
		return "", fmt.Errorf("sonoff: info state parse: %w", err)
	}
	if state.Switch == "" {
		return "", fmt.Errorf("sonoff: info no switch field")
	}
	return state.Switch, nil
}

func encryptParams(apiKey string, params []byte) (encrypted, iv []byte, err error) {
	h := md5.Sum([]byte(apiKey))
	key := h[:]

	iv = make([]byte, aes.BlockSize)
	if _, err = rand.Read(iv); err != nil {
		return nil, nil, fmt.Errorf("sonoff: iv: %w", err)
	}

	padded := pkcs7Pad(params, aes.BlockSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("sonoff: aes: %w", err)
	}
	encrypted = make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, padded)
	return encrypted, iv, nil
}

func decryptData(apiKey, ivB64, dataB64 string) ([]byte, error) {
	h := md5.Sum([]byte(apiKey))
	key := h[:]

	iv, err := base64.StdEncoding.DecodeString(ivB64)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("data not block-aligned")
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, data)
	return pkcs7Unpad(plain), nil
}

func sendCommand(ip, deviceID, apiKey string, on bool) error {
	switchState := "off"
	if on {
		switchState = "on"
	}

	params, _ := json.Marshal(map[string]any{"switch": switchState})
	encrypted, iv, err := encryptParams(apiKey, params)
	if err != nil {
		return err
	}

	body, _ := json.Marshal(map[string]any{
		"sequence":   fmt.Sprintf("%d", time.Now().UnixMilli()),
		"deviceid":   deviceID,
		"selfApikey": "###",
		"iv":         base64.StdEncoding.EncodeToString(iv),
		"encrypt":    true,
		"data":       base64.StdEncoding.EncodeToString(encrypted),
	})

	url := fmt.Sprintf("http://%s:8081/zeroconf/switch", ip)
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sonoff: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("sonoff: HTTP %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var reply struct {
		Error int `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err == nil && reply.Error != 0 {
		return fmt.Errorf("sonoff: switch error %d", reply.Error)
	}
	return nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	padded := make([]byte, len(data)+padding)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	return padded
}

func pkcs7Unpad(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(data) {
		return data
	}
	return data[:len(data)-pad]
}
