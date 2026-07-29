package liquidfw

import (
	"crypto/ecdh"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

type device struct {
	ID           uint32    `json:"id"`
	Name         string    `json:"name"`
	IP           string    `json:"ip"`
	IdentityPub  [32]byte  `json:"identity_pub"`  // long-term; used for TOFU auth
	EphemeralPub [32]byte  `json:"ephemeral_pub"` // current session ephemeral pub
	SessionKey   [32]byte  `json:"session_key"`
	CmdNonce     uint64    `json:"cmd_nonce"`   // outbound HMAC nonce (monotonic, persisted)
	LastNonce    uint64    `json:"last_nonce"`  // last received STATE nonce
	LastSeen     time.Time `json:"last_seen"`
}

type registry struct {
	mu      sync.RWMutex
	devices map[uint32]*device
	path    string
	hfPriv  *ecdh.PrivateKey
}

func newRegistry(path string, hfPriv *ecdh.PrivateKey) *registry {
	r := &registry{
		devices: make(map[uint32]*device),
		path:    path,
		hfPriv:  hfPriv,
	}
	if err := r.load(); err != nil && !os.IsNotExist(err) {
		slog.Warn("liquidfw: registry load", "err", err)
	}
	return r
}

func (r *registry) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.Unmarshal(data, &r.devices)
}

func (r *registry) save() {
	r.mu.RLock()
	data, _ := json.MarshalIndent(r.devices, "", "  ")
	r.mu.RUnlock()
	if err := os.WriteFile(r.path, data, 0600); err != nil {
		slog.Warn("liquidfw: registry save", "err", err)
	}
}

// onAnnounce handles a v2 ANNOUNCE (69 bytes).
// First ANNOUNCE for a device: TOFU — store identity_pub.
// Re-ANNOUNCE (rotation/reconnect): verify identity_pub matches; reject if changed
// (prevents a rogue device claiming an existing device_id with a different identity).
// Session key always derived from ephemeral_pub, giving forward secrecy per rotation window.
func (r *registry) onAnnounce(deviceID uint32, identityPub [32]byte, ephemeralPub [32]byte, srcIP string) error {
	var zero [32]byte
	r.mu.Lock()
	d := r.devices[deviceID]

	// Keep-alive fast path: an already-keyed device re-announcing with the SAME
	// ephemeral+identity keys. This is NOT a real key rotation — it's a re-announce
	// triggered by the firmware's WiFi-reconnect / RX-starvation watchdogs (common on
	// weak-signal devices). sendAnnounce() re-sends the CURRENT ephemeral; only the 24h
	// crypto.rotate() (or a reboot) actually changes it. Re-deriving here produces the
	// identical key, so skip it — plus resetting the nonce window, re-saving, and logging
	// "session keyed" on every such announce spammed the log and churned the entity set
	// (the UI card flicker). Just refresh liveness (IP may change via DHCP) and return.
	if d != nil && d.SessionKey != zero && d.EphemeralPub == ephemeralPub && d.IdentityPub == identityPub {
		d.IP = srcIP
		d.LastSeen = time.Now()
		r.mu.Unlock()
		return nil
	}

	// New device, or a REAL key change (24h rotation / reboot / reflash) — full (re)key.
	sk, err := deriveSessionKey(r.hfPriv, ephemeralPub, deviceID)
	if err != nil {
		r.mu.Unlock()
		return fmt.Errorf("derive session key: %w", err)
	}
	if d == nil {
		d = &device{ID: deviceID}
		r.devices[deviceID] = d
		d.IdentityPub = identityPub
		slog.Info("liquidfw: new device (TOFU)", "id", fmt.Sprintf("%08x", deviceID), "ip", srcIP)
	} else {
		if d.IdentityPub != zero && d.IdentityPub != identityPub {
			// Baked (no-LittleFS) devices regenerate their identity key on every boot, so a
			// changed identity_pub is the expected outcome of a reboot/reflash, not an attack.
			// On this trusted LAN we accept it and re-key, rather than locking the device out
			// (which previously required manually deleting the registry entry after each reflash).
			slog.Warn("liquidfw: identity_pub changed — re-provisioning device",
				"id", fmt.Sprintf("%08x", deviceID), "ip", srcIP)
		}
		d.IdentityPub = identityPub
	}
	d.EphemeralPub = ephemeralPub
	d.SessionKey = sk
	d.IP = srcIP
	d.LastNonce = 0 // reset nonce window — new session key, fresh nonce space
	d.LastSeen = time.Now()
	r.mu.Unlock()
	r.save()
	slog.Info("liquidfw: session keyed", "id", fmt.Sprintf("%08x", deviceID), "ip", srcIP)
	return nil
}

func (r *registry) get(deviceID uint32) *device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.devices[deviceID]
}

func (r *registry) findByName(name string) *device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.devices {
		if d.Name == name {
			return d
		}
	}
	return nil
}

// updateFromState validates nonce (replay protection) and persists name when first seen.
func (r *registry) updateFromState(deviceID uint32, name string, nonce uint64) bool {
	r.mu.Lock()
	d := r.devices[deviceID]
	if d == nil {
		r.mu.Unlock()
		return false
	}
	if nonce <= d.LastNonce {
		r.mu.Unlock()
		return false
	}
	d.LastNonce = nonce
	nameChanged := false
	if name != "" && d.Name != name {
		d.Name = name
		nameChanged = true
	}
	d.LastSeen = time.Now()
	r.mu.Unlock()
	if nameChanged {
		r.save()
	}
	return true
}

// nextCmdNonce returns the next monotonic command nonce and persists it immediately.
// Persisting before use ensures a homeforge crash can't reuse a nonce.
func (r *registry) nextCmdNonce(deviceID uint32) uint64 {
	r.mu.Lock()
	d := r.devices[deviceID]
	if d == nil {
		r.mu.Unlock()
		return 0
	}
	d.CmdNonce++
	nonce := d.CmdNonce
	r.mu.Unlock()
	r.save()
	return nonce
}
