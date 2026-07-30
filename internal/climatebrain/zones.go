package climatebrain

// Holistic per-room comfort — Stage 1: SENSE + OBSERVE (no actuation).
//
// The core idea (the user's): don't care whether a room's sensor is accurate. Use each gauge
// RELATIVE to itself. Bedroom2 reading 87 while the master probe reads 77 doesn't mean it's
// broken — 87 is just bedroom2's number, and the job is to move bedroom2's number down with
// bedroom2's own lever. A "just right" feel-tap pins each room's comfortable reading; the brain
// then holds each gauge near its own baseline and learns, by trial and error, how many degrees
// one fan step buys ON THAT GAUGE. Stage 1 only watches + publishes a per-room panel so the
// room↔sensor↔fan mapping and the baselines can be verified before anything is ever commanded.

import (
	"fmt"
	"time"

	"github.com/whimtrav/homeforge/internal/entity"
)

// zoneRuntime is the per-room running state.
type zoneRuntime struct {
	typicalEMA  float64 // slow EMA of the reading = provisional baseline until a feel-tap pins comfort
	haveEMA     bool
	comfort     float64 // learned comfortable reading from "just right" taps (0 = unset), in the gauge's own units
	haveComfort bool
	prev        float64   // reading ~15 min ago (for a simple trend)
	prevAt      time.Time
	trend15     float64 // °/15 min on this gauge (+ = warming)
}

// publishZones reads every configured room's own gauge, tracks a relative baseline + trend, and
// publishes sensor.climatebrain_zone_<name>. Observe-only: it never commands a fan in Stage 1.
func (m *Manager) publishZones(now time.Time) {
	if len(m.cfg.Zones) == 0 {
		return
	}
	if m.zones == nil {
		m.zones = make(map[string]*zoneRuntime)
	}
	const emaAlpha = 0.02 // ~ slow: "typical" reading over hours at a 60s tick

	for _, z := range m.cfg.Zones {
		raw, ok := m.num(z.TempSensor)
		if !ok {
			continue // sensor missing/stale this tick — skip, don't publish a stale room
		}
		readF := raw
		if z.TempIsC {
			readF = raw*9/5 + 32
		}
		feels := readF
		if z.Humidity != "" {
			if rh, ok := m.num(z.Humidity); ok && rh > 0 {
				feels = heatIndex(readF, rh)
			}
		}

		zr := m.zones[z.Name]
		if zr == nil {
			zr = &zoneRuntime{typicalEMA: readF, haveEMA: true, prev: readF, prevAt: now}
			m.zones[z.Name] = zr
		}
		// provisional "typical" baseline (until a comfort tap pins the real one)
		if zr.haveEMA {
			zr.typicalEMA += emaAlpha * (readF - zr.typicalEMA)
		} else {
			zr.typicalEMA, zr.haveEMA = readF, true
		}
		// simple 15-minute trend on this gauge
		if now.Sub(zr.prevAt) >= 15*time.Minute {
			zr.trend15 = readF - zr.prev
			zr.prev, zr.prevAt = readF, now
		}

		base, baseSrc := zr.typicalEMA, "typical (no feel-tap yet)"
		if zr.haveComfort {
			base, baseSrc = zr.comfort, "learned (feel-tap)"
		}
		drift := readF - base

		attrs := map[string]any{
			"device": "climate-brain", "section": "climate", "zone": z.Name,
			"reading_f": round1(readF), "feels_f": round1(feels),
			"baseline_f": round1(base), "baseline_src": baseSrc,
			"drift_f": round1(drift), "trend_15m": round1(zr.trend15),
			"observe_only": true,
		}
		if z.Fan != "" {
			attrs["fan_entity"] = z.Fan
			if fs, ok := m.num(z.Fan); ok {
				attrs["fan_speed"] = int(fs + 0.5)
			}
		}
		m.store.Set(entity.Entity{
			ID:         "sensor.climatebrain_zone_" + z.Name,
			Name:       "Climate Zone " + z.Name,
			Domain:     "sensor",
			State:      fmt.Sprintf("%.1f°F (%+.1f vs base %.1f)", readF, drift, base),
			Attributes: attrs,
		})
	}
}
