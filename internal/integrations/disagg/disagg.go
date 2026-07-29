// Package disagg is a HomeForge integration that disaggregates the single BL0939
// meter on the "washerdryer" DualR3 (192.168.1.10) into the THREE loads sharing that
// circuit: the LG washer/dryer combo, a sump pump, and an automatic cat litter box.
//
// It polls the device /state every 2s and runs a priority state machine keyed off the
// measured signatures (captured 2026-07-25, see the washerdryer-disaggregation memory):
//   - cat litter : ≤ ~4 W (brief 1-4 W hump ~60 s, then ~0.4 W standby)
//   - sump pump  : sharp FLAT ~300 W square, short (< ~3 min), hard on/off edges
//   - washer/dry : LONG (hours), variable multi-phase (wash → spin → ~700 W dry plateau)
//
// The three almost never overlap, so precedence works: a long sustained run = washer;
// a short flat mid-power burst (when the washer is idle) = sump; a tiny hump = litter.
// Publishes read-only binary_sensor/sensor entities the energy dashboard renders.
package disagg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

const (
	pollEvery = 2 * time.Second

	offThreshold = 15.0 // W: above the litter standby (~0.4W), below the wash motor

	// washer/dryer: a run this long (or a big peak) is unambiguously the washer, not a sump.
	washerConfirm  = 8 * time.Minute
	washerBigPeak  = 550.0           // dry-heat/spin territory — no other load reaches it
	washerEndQuiet = 5 * time.Minute // circuit quiet this long ⇒ the (multi-phase) cycle is done

	// sump pump: sharp, flat, short. Dry-run reference was ~300W; a wet run may draw a bit
	// more, so the band is generous. Flatness (avg/peak) separates it from the variable wash motor.
	sumpMinW    = 150.0
	sumpMaxW    = 520.0
	sumpMaxDur  = 4 * time.Minute
	sumpMinDur  = 4 * time.Second
	sumpFlatMin = 0.70 // avg/peak; sump is a flat square (~1.0), wash motor is variable (~0.5)

	// cat litter: a brief hump ABOVE the idle baseline. The combo has a small, drifting
	// standby draw (~1W after a cycle) on top of the litter's ~0.4W standby, so litter must
	// be detected RELATIVE to a tracked floor — an absolute band latches on the standby.
	litterRise   = 1.2 // W above baseline to call it a cycle
	litterAbove  = 8.0 // W above baseline ceiling (a real cycle peaks ~3-4W over)
	litterClear  = 0.6 // W above baseline to end the cycle
	litterMinDur = 8 * time.Second
	litterMaxDur = 3 * time.Minute

	sumpAlertPerHour = 6 // sump runs/hr above this ⇒ possible water intrusion
)

var httpc = &http.Client{Timeout: 3 * time.Second}

type Manager struct {
	cfg   config.DisaggConfig
	store *entity.Store

	// current above-threshold run (candidate short-load, before washer is confirmed)
	actSince  time.Time
	actLastOn time.Time
	actPeak   float64
	actEnergy float64 // watt-seconds

	// washer/dryer state
	washer       bool
	washerEnergy float64 // watt-seconds this cycle
	washerQuiet  time.Time
	lastCycleKwh float64
	lastCycleEnd time.Time

	// litter hump tracking (relative to a tracked idle baseline)
	litSince time.Time
	ring     []float64 // recent idle-band samples → baseline floor for litter detection

	// counters (reset at local midnight; in-memory → reset on HF restart)
	day       int
	sumpToday int
	litToday  int
	sumpLast  time.Time
	sumpTimes []time.Time
}

func NewManager(cfg config.DisaggConfig, store *entity.Store) *Manager {
	return &Manager{cfg: cfg, store: store, day: -1}
}

func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("disagg: disabled")
		<-ctx.Done()
		return
	}
	host := m.cfg.MeterHost
	if host == "" {
		host = "192.168.1.10"
	}
	slog.Info("disagg: starting", "meter_host", host)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if p, ok := readPower(host); ok {
				m.tick(p, now)
			}
		}
	}
}

func readPower(host string) (float64, bool) {
	resp, err := httpc.Get("http://" + host + "/state")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, false
	}
	var s struct {
		IO struct {
			Meter struct {
				Power1 float64 `json:"power1"`
			} `json:"meter"`
		} `json:"io"`
	}
	if json.Unmarshal(b, &s) != nil {
		return 0, false
	}
	return s.IO.Meter.Power1, true
}

func (m *Manager) tick(p float64, now time.Time) {
	if d := now.YearDay(); d != m.day {
		m.day, m.sumpToday, m.litToday = d, 0, 0
	}
	dt := pollEvery.Seconds()

	if m.washer {
		// integrate the whole cycle; end only after a long quiet gap (bridges inter-phase dips)
		m.washerEnergy += p * dt
		if p < offThreshold {
			if m.washerQuiet.IsZero() {
				m.washerQuiet = now
			} else if now.Sub(m.washerQuiet) >= washerEndQuiet {
				m.lastCycleKwh = m.washerEnergy / 3.6e6
				m.lastCycleEnd = now
				slog.Info("disagg: washer/dryer cycle finished", "kwh", fmt.Sprintf("%.2f", m.lastCycleKwh))
				m.washer, m.washerEnergy, m.washerQuiet = false, 0, time.Time{}
			}
		} else {
			m.washerQuiet = time.Time{}
		}
		m.publish(p, now)
		return
	}

	if p > offThreshold {
		// an above-threshold run is in progress — accumulate it
		if m.actSince.IsZero() {
			m.actSince, m.actPeak, m.actEnergy = now, 0, 0
		}
		if p > m.actPeak {
			m.actPeak = p
		}
		m.actEnergy += p * dt
		m.actLastOn = now
		// promote to washer once it's clearly a marathon or hits big-load territory
		if m.actPeak >= washerBigPeak || now.Sub(m.actSince) >= washerConfirm {
			slog.Info("disagg: washer/dryer started")
			m.washer, m.washerEnergy, m.washerQuiet = true, m.actEnergy, time.Time{}
			m.actSince = time.Time{}
		}
	} else {
		// circuit dropped below threshold — finalize any short run as a sump event
		if !m.actSince.IsZero() {
			m.classifyShortRun(now)
			m.actSince = time.Time{}
		}
		// litter hump tracking, RELATIVE to the idle baseline (min over ~5min of idle
		// samples) so the combo's drifting standby draw doesn't read as a permanent cycle.
		m.ring = append(m.ring, p)
		if len(m.ring) > 150 { // ~5 min at 2s
			m.ring = m.ring[len(m.ring)-150:]
		}
		base := m.ring[0]
		for _, v := range m.ring {
			if v < base {
				base = v
			}
		}
		if p >= base+litterRise && p <= base+litterAbove {
			if m.litSince.IsZero() {
				m.litSince = now
			}
		} else if p < base+litterClear && !m.litSince.IsZero() {
			if d := now.Sub(m.litSince); d >= litterMinDur && d <= litterMaxDur {
				m.litToday++
				slog.Info("disagg: cat litter cycle", "dur_s", int(d.Seconds()))
			}
			m.litSince = time.Time{}
		}
	}
	m.publish(p, now)
}

// classifyShortRun decides whether a just-ended above-threshold run was the sump pump:
// a flat (avg≈peak), mid-power, short-duration square. Anything else is ignored as a blip.
func (m *Manager) classifyShortRun(now time.Time) {
	dur := m.actLastOn.Sub(m.actSince)
	if dur < sumpMinDur || dur > sumpMaxDur {
		return
	}
	if m.actPeak < sumpMinW || m.actPeak > sumpMaxW {
		return
	}
	avg := m.actEnergy / dur.Seconds()
	if avg/m.actPeak < sumpFlatMin {
		return // too variable to be the flat sump square (probably a brief washer poke)
	}
	m.sumpToday++
	m.sumpLast = now
	m.sumpTimes = append(m.sumpTimes, now)
	slog.Info("disagg: sump pump run", "dur_s", int(dur.Seconds()), "peak_w", int(m.actPeak))
}

func phase(p float64) string {
	switch {
	case p < 50:
		return "fill"
	case p < 250:
		return "wash"
	case p < 600:
		return "rinse/spin"
	default:
		return "dry"
	}
}

func (m *Manager) publish(p float64, now time.Time) {
	set := func(id, name, domain, state string, attr map[string]any) {
		if attr == nil {
			attr = map[string]any{}
		}
		attr["device"], attr["section"] = "laundry-circuit", "appliance"
		m.store.Set(entity.Entity{ID: id, Name: name, Domain: domain, State: state, Attributes: attr})
	}
	onoff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}

	// washer/dryer
	set("binary_sensor.washerdryer_running", "Washer/Dryer Running", "binary_sensor", onoff(m.washer), nil)
	ph := "idle"
	if m.washer {
		ph = phase(p)
	}
	set("sensor.washerdryer_phase", "Washer/Dryer Phase", "sensor", ph, nil)
	cur := 0.0
	if m.washer {
		cur = m.washerEnergy / 3.6e6
	}
	set("sensor.washerdryer_cycle_energy", "Washer/Dryer Cycle Energy", "sensor",
		fmt.Sprintf("%.2f", cur), map[string]any{"unit_of_measurement": "kWh"})
	set("sensor.washerdryer_last_cycle_energy", "Washer/Dryer Last Cycle", "sensor",
		fmt.Sprintf("%.2f", m.lastCycleKwh), map[string]any{"unit_of_measurement": "kWh"})

	// sump pump
	sumpActive := !m.actSince.IsZero() && m.actPeak >= sumpMinW && m.actPeak <= sumpMaxW
	set("binary_sensor.sump_pump_running", "Sump Pump Running", "binary_sensor", onoff(sumpActive), nil)
	set("sensor.sump_pump_runs_today", "Sump Pump Runs Today", "sensor", strconv.Itoa(m.sumpToday), nil)
	last := "never"
	if !m.sumpLast.IsZero() {
		last = m.sumpLast.Format("Jan 2 15:04")
	}
	set("sensor.sump_pump_last_run", "Sump Pump Last Run", "sensor", last, nil)
	cut, n := now.Add(-time.Hour), 0
	for _, t := range m.sumpTimes {
		if t.After(cut) {
			n++
		}
	}
	set("binary_sensor.sump_pump_alert", "Sump Pump Alert", "binary_sensor", onoff(n >= sumpAlertPerHour),
		map[string]any{"runs_last_hour": n})

	// cat litter
	set("binary_sensor.cat_litter_running", "Cat Litter Running", "binary_sensor", onoff(!m.litSince.IsZero()), nil)
	set("sensor.cat_litter_cycles_today", "Cat Litter Cycles Today", "sensor", strconv.Itoa(m.litToday), nil)

	// summary: what's on the shared circuit right now
	sum := "idle"
	switch {
	case m.washer:
		sum = "washer/dryer"
	case sumpActive:
		sum = "sump pump"
	case !m.litSince.IsZero():
		sum = "cat litter"
	case p > offThreshold:
		sum = "unknown load"
	}
	set("sensor.laundry_circuit", "Laundry Circuit", "sensor", sum, map[string]any{"power_w": int(p)})
}
