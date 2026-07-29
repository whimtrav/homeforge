package liquidfw

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
)

const (
	pktAnnounce = 0x00
	pktState    = 0x01
	pktAck      = 0x03
)

// statePacket matches the JSON structure produced by state.h buildJSON():
//
//	{ "name":"...", "uptime":123, "rssi":-65, "nonce":5,
//	  "io": { "led": false, "temp_sensor": {"temp": 23.5, "humidity": 60.2} } }
type statePacket struct {
	Name   string         `json:"name"`
	Uptime uint32         `json:"uptime"`
	RSSI   int            `json:"rssi"`
	Nonce  uint64         `json:"nonce"`
	IO     map[string]any `json:"io"`
}

func listenUDP(port int, reg *registry, onState func(deviceID uint32, s statePacket, srcIP string)) error {
	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("bind udp :%d: %w", port, err)
	}
	defer conn.Close()
	slog.Info("liquidfw: UDP listener", "port", port)

	buf := make([]byte, 1500)
	for {
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			slog.Warn("liquidfw: udp read", "err", err)
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		srcIP := src.(*net.UDPAddr).IP.String()
		go dispatch(pkt, srcIP, src, conn, reg, onState)
	}
}

// sendAck replies [pktAck][4B device_id LE] to the address a packet came from.
// Devices use ACK arrival as proof their WiFi RX path is alive; >3 min without
// one triggers their reassociation watchdog (L3-dead self-heal).
func sendAck(conn net.PacketConn, dst net.Addr, deviceID uint32) {
	ack := make([]byte, 5)
	ack[0] = pktAck
	binary.LittleEndian.PutUint32(ack[1:5], deviceID)
	if _, err := conn.WriteTo(ack, dst); err != nil {
		slog.Debug("liquidfw: ack send", "dst", dst.String(), "err", err)
	}
}

func dispatch(pkt []byte, srcIP string, src net.Addr, conn net.PacketConn, reg *registry, onState func(uint32, statePacket, string)) {
	if len(pkt) < 1 {
		return
	}

	// Plaintext JSON fallback — device has no homeforge key yet
	if pkt[0] == '{' {
		var s statePacket
		if err := json.Unmarshal(pkt, &s); err != nil {
			slog.Warn("liquidfw: bad plaintext json", "src", srcIP, "err", err)
			return
		}
		slog.Debug("liquidfw: plain state", "src", srcIP, "name", s.Name, "uptime", s.Uptime)
		sendAck(conn, src, 0)
		if onState != nil {
			onState(0, s, srcIP)
		}
		return
	}

	switch pkt[0] {
	case pktAnnounce:
		if len(pkt) == 37 {
			// v1 format — device needs firmware update
			slog.Warn("liquidfw: v1 announce (37 bytes) — device needs firmware update for key rotation support", "src", srcIP)
			return
		}
		if len(pkt) != 69 {
			slog.Warn("liquidfw: bad announce", "src", srcIP, "len", len(pkt))
			return
		}
		deviceID := binary.LittleEndian.Uint32(pkt[1:5])
		var identityPub [32]byte
		var ephemeralPub [32]byte
		copy(identityPub[:], pkt[5:37])
		copy(ephemeralPub[:], pkt[37:69])
		if err := reg.onAnnounce(deviceID, identityPub, ephemeralPub, srcIP); err != nil {
			slog.Error("liquidfw: announce", "err", err)
		} else {
			sendAck(conn, src, deviceID)
		}

	case pktState:
		if len(pkt) < 5+12+16 {
			slog.Warn("liquidfw: state too short", "src", srcIP, "len", len(pkt))
			return
		}
		deviceID := binary.LittleEndian.Uint32(pkt[1:5])
		d := reg.get(deviceID)
		if d == nil {
			slog.Debug("liquidfw: unknown device, waiting for ANNOUNCE", "id", fmt.Sprintf("%08x", deviceID))
			return
		}
		// counter is the last 8 bytes of the 12-byte nonce (bytes 9-17 in packet)
		counter := binary.LittleEndian.Uint64(pkt[9:17])

		plain, err := decryptState(d.SessionKey, pkt)
		if err != nil {
			slog.Warn("liquidfw: decrypt fail", "id", fmt.Sprintf("%08x", deviceID), "err", err)
			return
		}
		var s statePacket
		if err := json.Unmarshal(plain, &s); err != nil {
			slog.Warn("liquidfw: bad json post-decrypt", "id", fmt.Sprintf("%08x", deviceID), "err", err)
			return
		}
		if !reg.updateFromState(deviceID, s.Name, counter) {
			slog.Debug("liquidfw: nonce replay", "id", fmt.Sprintf("%08x", deviceID), "nonce", counter)
			return
		}
		ioJSON, _ := json.Marshal(s.IO)
		slog.Info("liquidfw: state", "device", s.Name, "nonce", counter, "uptime", s.Uptime, "rssi", s.RSSI, "io", string(ioJSON))
		sendAck(conn, src, deviceID)
		if onState != nil {
			onState(deviceID, s, srcIP)
		}

	default:
		slog.Debug("liquidfw: unknown pkt type", "type", fmt.Sprintf("0x%02x", pkt[0]), "src", srcIP)
	}
}
