package mqtt

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	mqttclient "github.com/eclipse/paho.mqtt.golang"
	"github.com/whimtrav/homeforge/internal/entity"
)

// The Droplet reports flow in LITERS/min over MQTT (verified against a timed known-volume
// pour: 1.000 gal @ 141 s → 3.69 raw units, and 3.69 / 3.785 = 0.975 gal). So we convert
// raw → gallons up front, then everything downstream is real gallons.
//
// Cumulative total = trapezoidal integration of that (gal/min) flow over real elapsed
// time. The device's own "volume" MQTT field is pure noise (−99..+1734, negatives), so
// volume is derived from flow, not read. A per-pour "last event" volume is also tracked
// (a flow session: flow rises above the deadband → returns to zero, debounced so a single
// dropout sample doesn't split it).
//
// dropletCalibration is a residual multiplier (~1.0); the unit conversion does the real
// work, so leave it at 1.0 unless timed pours show a consistent bias.
const (
	litersPerGallon     = 3.785411784
	dropletCalibration  = 1.0
	dropletFlowDeadband = 0.05 // gal/min below this = no flow (kills idle noise)
	dropletOnsetCapSec  = 5.0  // cap the idle-gap dt at flow onset to limit overcount
)

type dropletAccum struct {
	total     float64
	session   float64
	last      time.Time
	lastFlow  float64
	inSession bool
	prevZero  bool // last reading was zero flow (debounces session-end)
	seeded    bool
}

var (
	dropletMu    sync.Mutex
	dropletTotal = map[string]*dropletAccum{}
)

// handleDroplet ingests the Hydrific Droplet water sensor's plain (non-discovery) MQTT:
//
//	droplet-<id>/state  = {"server","signal","volume","flow"}
//	droplet-<id>/leak   = {"high_leak","low_leak"}   ("ON"/"OFF")
//	droplet-<id>/health = "online"
func (s *Server) handleDroplet(_ mqttclient.Client, msg mqttclient.Message) {
	parts := strings.SplitN(msg.Topic(), "/", 2)
	if len(parts) != 2 {
		return
	}
	dev, sub := parts[0], parts[1]
	id := sanitizeID(strings.TrimPrefix(dev, "droplet-")) // "droplet-FE5C" -> "fe5c"
	name := strings.ReplaceAll(dev, "-", " ")

	setSensor := func(key, state, unit, devClass string) {
		attrs := map[string]any{"source": "droplet", "device": dev, "section": "water"}
		if unit != "" {
			attrs["unit_of_measurement"] = unit
		}
		if devClass != "" {
			attrs["device_class"] = devClass
		}
		s.store.Set(entity.Entity{
			ID:     "sensor.droplet_" + id + "_" + key,
			Name:   name + " " + strings.ReplaceAll(key, "_", " "),
			Domain: "sensor", State: state, Attributes: attrs,
		})
	}
	setBinary := func(key, state, devClass string) {
		attrs := map[string]any{"source": "droplet", "device": dev, "section": "water"}
		if devClass != "" {
			attrs["device_class"] = devClass
		}
		s.store.Set(entity.Entity{
			ID:     "binary_sensor.droplet_" + id + "_" + key,
			Name:   name + " " + strings.ReplaceAll(key, "_", " "),
			Domain: "binary_sensor", State: state, Attributes: attrs,
		})
	}
	onOff := func(v string) string {
		if strings.EqualFold(strings.TrimSpace(v), "on") {
			return "on"
		}
		return "off"
	}

	switch sub {
	case "state":
		var d struct {
			Server string  `json:"server"`
			Signal string  `json:"signal"`
			Volume float64 `json:"volume"`
			Flow   float64 `json:"flow"`
		}
		if json.Unmarshal(msg.Payload(), &d) != nil {
			return
		}
		flowGal := d.Flow / litersPerGallon  // device reports L/min → gal/min
		volGal := d.Volume / litersPerGallon // (noise, but keep units consistent)
		setSensor("flow", strconv.FormatFloat(flowGal, 'f', 3, 64), "gal/min", "volume_flow_rate")
		setSensor("volume", strconv.FormatFloat(volGal, 'f', 3, 64), "gal", "water")

		f := flowGal
		if f < dropletFlowDeadband {
			f = 0 // idle-noise deadband
		}

		dropletMu.Lock()
		a := dropletTotal[id]
		if a == nil {
			a = &dropletAccum{}
			dropletTotal[id] = a
		}
		now := time.Now()
		lastEvent := -1.0
		if !a.seeded {
			// Continue the persisted total across restarts. Prefer the live entity, but on
			// a fresh boot it isn't materialized yet — fall back to the boot snapshot, or the
			// accumulator would start at 0 and clobber the restored total on the next write.
			key := "sensor.droplet_" + id + "_total"
			seedStr := ""
			if e, ok := s.store.Get(key); ok {
				seedStr = e.State
			} else if sv, ok := s.store.Restored(key); ok {
				seedStr = sv
			}
			if v, err := strconv.ParseFloat(seedStr, 64); err == nil {
				a.total = v
			}
			a.seeded = true
		} else {
			if f > 0 && !a.inSession { // pour starts
				a.inSession = true
				a.session = 0
			}
			dt := now.Sub(a.last).Seconds()
			if a.lastFlow == 0 && dt > dropletOnsetCapSec {
				dt = dropletOnsetCapSec // don't charge a quiet→flow jump for the whole idle gap
			}
			if dt > 0 && dt < 3600 {
				delta := (a.lastFlow + f) / 2.0 * dt / 60.0 * dropletCalibration // trapezoidal, gal
				if delta > 0 {
					a.total += delta
					if a.inSession {
						a.session += delta
					}
				}
			}
			// pour ends on two consecutive zero readings (single-sample dropouts survive)
			z := f == 0
			if a.inSession && z && a.prevZero {
				a.inSession = false
				lastEvent = a.session
			}
			a.prevZero = z
		}
		a.last = now
		a.lastFlow = f
		total := a.total
		dropletMu.Unlock()

		s.store.Set(entity.Entity{
			ID: "sensor.droplet_" + id + "_total", Name: name + " total",
			Domain: "sensor", State: strconv.FormatFloat(total, 'f', 3, 64),
			Attributes: map[string]any{"source": "droplet", "device": dev, "section": "water",
				"unit_of_measurement": "gal", "device_class": "water", "state_class": "total_increasing"},
		})
		if lastEvent >= 0 {
			s.store.Set(entity.Entity{
				ID: "sensor.droplet_" + id + "_last_event", Name: name + " last event",
				Domain: "sensor", State: strconv.FormatFloat(lastEvent, 'f', 3, 64),
				Attributes: map[string]any{"source": "droplet", "device": dev, "section": "water",
					"unit_of_measurement": "gal", "device_class": "water"},
			})
		}
		if d.Signal != "" {
			setSensor("signal", d.Signal, "", "enum")
		}
		if d.Server != "" {
			setSensor("server", d.Server, "", "enum")
		}
	case "leak":
		var d struct {
			High string `json:"high_leak"`
			Low  string `json:"low_leak"`
		}
		if json.Unmarshal(msg.Payload(), &d) != nil {
			return
		}
		setBinary("high_leak", onOff(d.High), "problem")
		setBinary("low_leak", onOff(d.Low), "problem")
	case "health":
		v := "off"
		if strings.EqualFold(strings.TrimSpace(string(msg.Payload())), "online") {
			v = "on"
		}
		setBinary("online", v, "connectivity")
	}
}
