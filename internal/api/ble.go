package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/whimtrav/homeforge/internal/entity"
)

// bleObs is one advertisement observed by a LiquidFW ESP32 scanner in one window.
type bleObs struct {
	MAC  string `json:"mac"`
	RSSI int    `json:"rssi"`
	Name string `json:"name,omitempty"`
	Mfg  string `json:"mfg,omitempty"`
	Svc  string `json:"svc,omitempty"`
}

func bleSanitize(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	var b strings.Builder
	for _, c := range k {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// decodeInkbird detects an Inkbird / iBBQ BBQ thermometer that broadcasts its probe temps in the
// BLE manufacturer data, and publishes sensor.inkbird_probeN — PASSIVELY, no connection needed
// (the device advertises the temps whenever it's powered on). Confirmed on a 4-probe unit:
//
//	mfg = [4-byte header][6-byte device MAC, forward][probe × int16 LE, °C×10]
//	e.g. 00000000 fc45c3897ec7 dc00 fa00 d200 d200  →  22.0 25.0 21.0 21.0 °C
//
// The header's 4th byte flips (0x80 ↔ 0x00) with app-connection state; temps are unaffected.
// A probe reading of 0xFFF0+ (or an absurd value) means that probe isn't plugged in.
func (s *Server) decodeInkbird(o bleObs) bool {
	mfg, err := hex.DecodeString(o.Mfg)
	if err != nil || len(mfg) < 12 {
		return false
	}
	macBytes, err := hex.DecodeString(strings.ReplaceAll(strings.ToLower(o.MAC), ":", ""))
	if err != nil || len(macBytes) != 6 {
		return false
	}
	// Signature: the device embeds its own MAC (forward) at mfg[4:10]. This is specific enough to
	// distinguish it from other MAC-embedding advertisers (which use a shorter frame / other offset).
	if !bytes.Equal(mfg[4:10], macBytes) {
		return false
	}
	n := (len(mfg) - 10) / 2
	if n < 1 || n > 8 {
		return false
	}
	for i := 0; i < n; i++ {
		raw := uint16(mfg[10+i*2]) | uint16(mfg[11+i*2])<<8
		attrs := map[string]any{
			"source": "inkbird", "section": "ble", "mac": o.MAC, "probe": i + 1,
			"rssi": o.RSSI, "device_class": "temperature", "unit_of_measurement": "°F",
		}
		state := "unavailable"
		if raw < 0xFFF0 && raw < 6000 { // plugged in + plausible (< 600 °C)
			c := float64(raw) / 10.0
			state = fmt.Sprintf("%.1f", c*9/5+32)
			attrs["celsius"] = float64(int(c*10+0.5)) / 10
			attrs["connected"] = true
		} else {
			attrs["connected"] = false
		}
		s.store.Set(entity.Entity{
			ID:     fmt.Sprintf("sensor.inkbird_probe%d", i+1),
			Name:   fmt.Sprintf("Inkbird Probe %d", i+1),
			Domain: "sensor", State: state, Attributes: attrs,
		})
	}
	return true
}

// handleBLE ingests one BLE scan window from a LiquidFW ESP32 scanner:
//
//	POST /api/ble/{device}   body = [{"mac","rssi","name","mfg","svc"}, ...]
//
// Discovery phase: we keep the whole window (sorted by RSSI, capped) in the
// scanner's count-entity attributes so everything it hears is visible from
// /api/entities, and log the named devices. Format decoders (sensors, presence,
// oral-b) come later once we see what's actually broadcasting.
func (s *Server) handleBLE(w http.ResponseWriter, r *http.Request) {
	scanner := bleSanitize(r.PathValue("device"))
	if scanner == "" {
		http.Error(w, "missing device", http.StatusBadRequest)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	trimmed := bytes.TrimSpace(raw)

	// Diagnostic object (posted once at boot): {"status","enabled","heap","largest"}.
	// Lets us see the BLE memory picture per device without serial access.
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var d map[string]any
		if err := json.Unmarshal(trimmed, &d); err != nil {
			http.Error(w, "bad diag", http.StatusBadRequest)
			return
		}
		slog.Info("ble: diag", "scanner", scanner, "status", d["status"],
			"enabled", d["enabled"], "attempted", d["attempted"], "reset", d["reset"],
			"heap", d["heap"], "largest", d["largest"])
		s.store.Set(entity.Entity{
			ID:     "sensor.ble_" + scanner + "_status",
			Name:   scanner + " BLE status",
			Domain: "sensor",
			State:  fmt.Sprintf("%v", d["status"]),
			Attributes: map[string]any{
				"source": "ble", "scanner": scanner, "section": "ble",
				"enabled": d["enabled"], "attempted": d["attempted"], "reset": d["reset"],
				"heap": d["heap"], "largest": d["largest"],
			},
		})
		w.WriteHeader(http.StatusOK)
		return
	}

	var obs []bleObs
	if err := json.Unmarshal(trimmed, &obs); err != nil {
		http.Error(w, "bad body (expected JSON array)", http.StatusBadRequest)
		return
	}
	sort.SliceStable(obs, func(i, j int) bool { return obs[i].RSSI > obs[j].RSSI })

	named, inkbird := 0, 0
	var sample []string
	for _, o := range obs {
		if o.Name != "" {
			named++
			if len(sample) < 8 {
				sample = append(sample, fmt.Sprintf("%s(%ddBm)", o.Name, o.RSSI))
			}
		}
		if s.decodeInkbird(o) {
			inkbird++
		}
	}
	slog.Info("ble: window", "scanner", scanner, "total", len(obs), "named", named,
		"inkbird_probes", inkbird, "top_named", strings.Join(sample, " "))

	top := obs
	if len(top) > 40 {
		top = top[:40]
	}
	s.store.Set(entity.Entity{
		ID:     "sensor.ble_" + scanner + "_count",
		Name:   scanner + " BLE devices",
		Domain: "sensor",
		State:  fmt.Sprintf("%d", len(obs)),
		Attributes: map[string]any{
			"source": "ble", "scanner": scanner, "section": "ble",
			"named": named, "observations": top,
		},
	})
	w.WriteHeader(http.StatusOK)
}
