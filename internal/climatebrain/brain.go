// Package climatebrain is the adaptive HVAC "brain": it EXPERIMENTS with the actuators,
// MEASURES the effect, and learns what target is efficiently ACHIEVABLE for THIS house —
// rather than chasing preset targets. It sits ABOVE the on-device thermostat (the safety
// layer) and only commands via safe service calls; it never bypasses compressor protection.
//
// Phase 1 (observe + experiment): per-tick SNAPSHOT logging → /data/climate-brain.jsonl,
// observability entities, and the ATTIC-FAN A/B experiment (toggle the exhaust fan in blocks,
// compare AC duty). No AC/setpoint actuation.
//
// Phase 2 (ACT on what it learned):
//   - SOLAR PRE-COOL ACTUATION — when exporting surplus solar in cool mode, nudge the cool
//     setpoint DOWN (bounded, never up, never below a hard floor) to bank coolth in the
//     thermal mass so the undersized 1.5-ton coasts the afternoon peak. User overrides win.
//   - ATTIC-FAN AUTO-RUN — once the A/B has a verdict that the fan cuts AC duty, just run it
//     during hot+sun instead of alternating.
//   - HUMIDITY-AWARE COMFORT — folds indoor RH (LiquidFW probes + VeSync humidifiers) into a
//     per-floor "feels-like", and learns a comfortable setpoint from the comfort-feedback taps.
package climatebrain

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

const (
	tickEvery    = 60 * time.Second
	stateFile    = "/data/climate-brain-state.json"
	comfortFile  = "/data/comfort-feedback.jsonl"
	overrideFile = "/data/setpoint-overrides.jsonl" // manual-override context log (comfort learning)
	comfortEvery = 10 // recompute the comfort model every N ticks (~10 min)
)

type Manager struct {
	cfg   config.ClimateBrainConfig
	store *entity.Store
	bus   *bus.Bus

	// attic-fan A/B experiment state
	arm      string // "" (idle) | "on" | "off" | "run" (learned auto-run)
	armStart time.Time
	armTicks int
	armCool  int

	// persisted learning + actuation state (saved to stateFile)
	onDutySum, offDutySum float64
	onN, offN             int
	atticVerdict          string // "" (learning) | "helps" | "no-help"

	baseSet     float64 // the user's intended cool setpoint (pre-cool restores to this)
	haveBase    bool
	precooling  bool    // brain is currently holding the setpoint below base
	precoolSince time.Time // when the current pre-cool session engaged (hysteresis + min-dwell)
	lastCmd     float64 // last setpoint the brain commanded (to detect user overrides)
	freeCooling bool    // brain is running the attic+box fans for free cooling (cooler out than in)

	// overnight fans-vs-AC test (night_fan): back the AC setpoint up toward a comfort cap and
	// run the attic+box fans, staying in cool mode so the AC is always armed as a fail-safe.
	nightFanning bool
	nfBaseSet    float64 // user's setpoint captured at test start (restored on exit)
	nfOverride   bool    // user changed the setpoint during the window → stand down
	nfLastCmd    float64 // last setback we commanded (override detection)

	warmHist []tempSample // recent warm-zone feels-like samples (unified-policy trend)

	// hot-day upstairs Heat-Guard: on a hot day, drive the setpoint DOWN to pull UPSTAIRS
	// (not the up/down average) to a comfort cap, and run the fans to break stratification.
	heatGuarding bool
	hgLastCmd    float64 // last setpoint Heat-Guard commanded (override detection)

	// holistic per-room comfort model (see zones.go) — per-room running state
	zones map[string]*zoneRuntime

	// manual-override hold: the unix-ts of the last user setpoint change we've already reacted to
	// (so we log/adopt each override once). See the manual-hold block in tick().
	lastOverrideTs int64
	holdStatus     string // non-empty while in a manual-override hold → shown as the brain status

	comfortSet float64 // learned comfortable setpoint (°F)
	comfortN   int

	tickN int
}

type persistState struct {
	OnDutySum    float64 `json:"on_duty_sum"`
	OffDutySum   float64 `json:"off_duty_sum"`
	OnN          int     `json:"on_n"`
	OffN         int     `json:"off_n"`
	AtticVerdict string  `json:"attic_verdict"`
	BaseSet      float64 `json:"base_set"`
	HaveBase     bool    `json:"have_base"`
	Precooling   bool    `json:"precooling"`
	LastCmd      float64 `json:"last_cmd"`
	NightFanning bool    `json:"night_fanning"`
	NfBaseSet    float64 `json:"nf_base_set"`
}

func NewManager(cfg config.ClimateBrainConfig, store *entity.Store, b *bus.Bus) *Manager {
	m := &Manager{cfg: cfg, store: store, bus: b}
	m.loadState()
	return m
}

func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("climatebrain: disabled")
		<-ctx.Done()
		return
	}
	slog.Info("climatebrain: starting", "attic_experiment", m.cfg.AtticExperiment,
		"precool_actuate", m.cfg.PrecoolActuate, "attic_auto_run", m.cfg.AtticAutoRun,
		"attic_verdict", m.atticVerdict)
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.tick(now)
		}
	}
}

func (m *Manager) tick(now time.Time) {
	m.tickN++

	// ── read signals ────────────────────────────────────────────────────────
	upC, haveUp := m.num("sensor.upstairs_temp_climate_temperature") // °C
	downC, haveDown := m.num("sensor.downstairs_temp_climate_temperature")
	upF, downF := upC*9/5+32, downC*9/5+32
	// optional cross-check upstairs temp (already °F, e.g. VeSync family-room humidifier)
	altF, haveAlt := 0.0, false
	if m.cfg.UpTempAlt != "" {
		altF, haveAlt = m.num(m.cfg.UpTempAlt)
	}
	// indoor humidity — probe + room RH (VeSync humidifiers) averaged
	upRH, upRHn := m.avgValid(append([]string{"sensor.upstairs_temp_climate_humidity"}, m.cfg.UpHumidity...))
	downRH, downRHn := m.avgValid(append([]string{"sensor.downstairs_temp_climate_humidity"}, m.cfg.DownHumidity...))
	// feels-like per floor (heat index)
	feelsUp, feelsDown := upF, downF
	if haveUp && upRHn > 0 {
		feelsUp = heatIndex(upF, upRH)
	}
	if haveDown && downRHn > 0 {
		feelsDown = heatIndex(downF, downRH)
	}

	outdoor, haveOut := m.num("sensor.outdoor_temperature") // °F
	pv, _ := m.num("sensor.solar_pv_power")
	grid, _ := m.num("sensor.solar_grid_power")
	batt, _ := m.num("sensor.solar_battery_state_of_charge")
	delta, haveDelta := m.num("sensor.climate_updown_delta") // °F, from the thermostat
	if !haveDelta && haveUp && haveDown {
		delta = math.Abs(upF - downF)
		haveDelta = true
	}

	mode, hvac := "", ""
	var setpoint, current float64
	var haveSet, haveCur bool
	spSource := ""
	var spChanged int64
	if e, ok := m.store.Get("climate.house"); ok {
		mode = e.State
		if a, ok := e.Attributes["hvac_action"].(string); ok {
			hvac = a
		}
		if v, ok := toF(e.Attributes["temperature"]); ok {
			setpoint, haveSet = v, true
		}
		if v, ok := toF(e.Attributes["current_temperature"]); ok {
			current, haveCur = v, true
		}
		// setpoint provenance stamped by the thermostat (who last moved it + when)
		spSource, _ = e.Attributes["setpoint_source"].(string)
		switch v := e.Attributes["setpoint_changed"].(type) {
		case int64:
			spChanged = v
		case int:
			spChanged = int64(v)
		case float64:
			spChanged = int64(v)
		}
	}
	atticFan := m.switchState(m.atticFanID())
	boxFan := m.switchState(m.boxFanID())
	cooling := hvac == "cooling"

	// ── manual-override hold: when the user sets the setpoint himself (UI/assistant), HIS value wins
	// for at least ManualHoldMin — the brain + all controllers hand off. Uses the provenance the
	// thermostat stamps on climate.house, so we never GUESS whether a change was his or ours. A new
	// override also becomes the authoritative base + gets its full context logged as a comfort signal.
	manualHold := false
	if (spSource == "user" || spSource == "assistant") && spChanged > 0 {
		holdSec := int64(orFloat(m.cfg.ManualHoldMin, 60) * 60)
		if now.Unix()-spChanged < holdSec {
			manualHold = true
		}
		if spChanged != m.lastOverrideTs { // a NEW manual override just happened
			m.lastOverrideTs = spChanged
			if haveSet {
				m.baseSet, m.haveBase = setpoint, true // his value is now the authoritative base
			}
			// clean hands-off: drop our controller state + stop our fans so nothing lingers or "restores"
			m.precooling, m.heatGuarding, m.nightFanning, m.freeCooling, m.arm = false, false, false, false, ""
			m.command(m.atticFanID(), false)
			m.command(m.boxFanID(), false)
			m.logOverride(now, spSource, setpoint, feelsUp, feelsDown, haveOut, outdoor)
			slog.Info("climatebrain: MANUAL override — handing off", "source", spSource,
				"setpoint", round1(setpoint), "hold_min", orFloat(m.cfg.ManualHoldMin, 60))
		}
	}

	qualify := false
	m.holdStatus = ""
	if manualHold {
		// Hands off — the brain observes only; the on-device thermostat holds the user's setpoint.
		// (Status is applied in publish() so nothing overwrites it later in the tick.)
		mins := int64(orFloat(m.cfg.ManualHoldMin, 60)) - (now.Unix()-spChanged)/60
		if mins < 0 {
			mins = 0
		}
		m.holdStatus = fmt.Sprintf("MANUAL HOLD — you set %.0f°F; brain resumes in ~%d min", setpoint, mins)
	} else {
		// ── night fan test: overnight, back off the AC toward a comfort cap + run the fans (the
		// "can fans hold it without the compressor" experiment). Stays in cool mode (AC armed).
		nightActive := m.handleNightFan(now, haveOut, outdoor, haveUp, upF, haveSet, setpoint, haveCur, current)
		if nightActive {
			m.freeCooling = false
			m.arm = ""
		} else {
			// ── free cooling: cooler outside than in → run the attic+box fans, let the AC cycle off.
			target := 74.0
			if haveSet {
				target = setpoint
			} else if m.haveBase {
				target = m.baseSet
			}
			freeCoolActive := m.handleFreeCool(haveOut, outdoor, haveUp, upF, target)
			// ── attic fan: experiment until verdict, then auto-run if it helps ────────
			qualify = m.cfg.AtticExperiment && haveOut && haveUp && outdoor > upF+1 && pv > 200
			if freeCoolActive {
				m.arm = "" // free-cool owns the fans; not in an A/B arm
			} else {
				m.handleAttic(now, qualify, upF, cooling)
			}
			// ── Heat-Guard (disabled via config) + solar pre-cool ─────────────────────
			guarding := m.handleHeatGuard(haveOut, outdoor, haveUp, upF, haveSet, setpoint)
			if guarding {
				m.precooling = false
			} else {
				m.handlePrecool(mode, haveSet, setpoint, haveCur, current, grid, haveOut, outdoor)
			}
		}
	}

	// ── unified policy: one objective loop, shadow unless policy_actuate ──────
	if m.cfg.Policy {
		m.runPolicy(now, feelsUp, feelsDown, haveOut, outdoor, grid)
	}

	// ── comfort model (periodic) ─────────────────────────────────────────────
	if m.cfg.ComfortModel && (m.tickN%comfortEvery == 1) {
		m.learnComfort()
	}

	// ── snapshot → training data ─────────────────────────────────────────────
	m.logSnapshot(now, snapshot{
		haveUp: haveUp, upF: upF, haveDown: haveDown, downF: downF, haveAlt: haveAlt, altF: altF,
		upRH: upRH, upRHn: upRHn, downRH: downRH, downRHn: downRHn, feelsUp: feelsUp, feelsDown: feelsDown,
		haveDelta: haveDelta, delta: delta, haveOut: haveOut, outdoor: outdoor,
		pv: pv, grid: grid, batt: batt, hvac: hvac, atticFan: atticFan, boxFan: boxFan,
	})

	// ── observability ────────────────────────────────────────────────────────
	m.publish(now, haveDelta, delta, haveOut, outdoor, upF, qualify,
		haveUp, feelsUp, upRH, upRHn, haveDown, feelsDown, downRH, downRHn)

	// ── holistic per-room comfort panel (Stage 1: observe only, no actuation) ──
	m.publishZones(now)

	m.saveState()
}

// ── attic fan ────────────────────────────────────────────────────────────────

func (m *Manager) handleAttic(now time.Time, qualify bool, upF float64, cooling bool) {
	if qualify {
		switch {
		case m.atticVerdict == "": // still learning → run the A/B
			m.runAtticExperiment(now, upF, cooling)
		case m.cfg.AtticAutoRun && m.atticVerdict == "helps": // learned it helps → just run it
			m.arm = "run"
			m.command(m.atticFanID(), true)
		default: // verdict says no-help, or auto-run disabled → leave it off
			m.arm = ""
			m.command(m.atticFanID(), false)
		}
		return
	}
	// conditions gone → end any live arm, turn the fan off
	if m.arm != "" || m.switchState(m.atticFanID()) == "on" {
		if (m.arm == "on" || m.arm == "off") && m.atticVerdict == "" {
			m.endArm(now)
		}
		m.arm = ""
		m.command(m.atticFanID(), false)
	}
}

func (m *Manager) runAtticExperiment(now time.Time, upF float64, cooling bool) {
	block := time.Duration(orInt(m.cfg.BlockMin, 30)) * time.Minute
	if m.arm != "on" && m.arm != "off" { // (re)start — control arm (fan OFF) first
		m.arm, m.armStart, m.armTicks, m.armCool = "off", now, 0, 0
		m.command(m.atticFanID(), false)
	}
	if now.Sub(m.armStart) >= block { // block done → score it, then flip arms
		m.endArm(now)
		if m.arm == "off" {
			m.arm = "on"
		} else {
			m.arm = "off"
		}
		m.armStart, m.armTicks, m.armCool = now, 0, 0
		m.command(m.atticFanID(), m.arm == "on")
	}
	m.armTicks++
	if cooling {
		m.armCool++
	}
}

func (m *Manager) endArm(now time.Time) {
	if m.armTicks < 3 { // too short to be meaningful
		return
	}
	duty := float64(m.armCool) / float64(m.armTicks)
	if m.arm == "on" {
		m.onDutySum += duty
		m.onN++
	} else {
		m.offDutySum += duty
		m.offN++
	}
	slog.Info("climatebrain: attic arm done", "arm", m.arm, "ac_duty", pct(duty),
		"on_avg", m.avg(m.onDutySum, m.onN), "off_avg", m.avg(m.offDutySum, m.offN))
	m.checkAtticVerdict()
}

// checkAtticVerdict decides, once there's enough data, whether the fan measurably helps.
func (m *Manager) checkAtticVerdict() {
	if m.atticVerdict != "" || m.onN < 3 || m.offN < 3 {
		return // need ≥3 arms each side — a 2-sample "verdict" is just noise
	}
	onAvg := m.onDutySum / float64(m.onN)
	offAvg := m.offDutySum / float64(m.offN)
	margin := orFloat(m.cfg.AtticMargin, 0.05)
	switch {
	case onAvg <= offAvg-margin:
		m.atticVerdict = "helps"
	case onAvg >= offAvg:
		m.atticVerdict = "no-help"
	case m.onN+m.offN >= 8: // marginal after plenty of data → not worth it
		m.atticVerdict = "no-help"
	}
	if m.atticVerdict != "" {
		slog.Info("climatebrain: attic verdict", "verdict", m.atticVerdict,
			"on_avg", pct(onAvg), "off_avg", pct(offAvg))
	}
}

// ── free cooling ─────────────────────────────────────────────────────────────

// handleFreeCool runs the attic exhaust + box fans when it's meaningfully cooler outside
// than upstairs and the house is above the setpoint — pulling cool outside air through so
// the AC can cycle off (the wasted overnight/evening window). It NEVER commands the AC:
// the on-device thermostat still holds comfort if the fans can't keep up. Returns true
// while active. Hysteresis (start ≥Δ cooler, stop when within 2°F) prevents flapping.
func (m *Manager) handleFreeCool(haveOut bool, outdoor float64, haveUp bool, upF, target float64) bool {
	if !m.cfg.FreeCool || !haveOut || !haveUp {
		m.stopFreeCool()
		return false
	}
	// Don't pull in muggy air — if it's humid out, free cooling just adds latent load.
	if maxRH := orFloat(m.cfg.FreeCoolMaxRH, 85); maxRH > 0 {
		if orh, ok := m.num("sensor.outdoor_humidity"); ok && orh > maxRH {
			m.stopFreeCool()
			return false
		}
	}
	startDelta := orFloat(m.cfg.FreeCoolDelta, 4)
	adv := upF - outdoor // how much cooler it is outside than upstairs
	var want bool
	if m.freeCooling {
		// keep going until upstairs reaches the setpoint OR the outdoor advantage shrinks
		want = upF > target && adv >= 2
	} else {
		// start only when it's clearly cooler out AND the house actually wants cooling
		want = upF > target+0.5 && adv >= startDelta
	}
	if !want {
		m.stopFreeCool()
		return false
	}
	if !m.freeCooling {
		slog.Info("climatebrain: free-cooling START", "upstairs", round1(upF), "outdoor", round1(outdoor),
			"advantage", round1(adv), "target", round1(target))
	}
	m.freeCooling = true
	m.command(m.atticFanID(), true)
	m.command(m.boxFanID(), true)
	return true
}

func (m *Manager) stopFreeCool() {
	if !m.freeCooling {
		return
	}
	m.freeCooling = false
	m.command(m.atticFanID(), false)
	m.command(m.boxFanID(), false)
	slog.Info("climatebrain: free-cooling END")
}

// ── night fan test ─────────────────────────────────────────────────────────
//
// handleNightFan runs the overnight "can fans hold it?" experiment. During the night window,
// when it's below the outdoor cutoff, it backs the AC setpoint UP toward a comfort cap and
// runs the attic + box fans, so we can measure whether the fans (not the compressor) keep the
// house comfortable. It deliberately stays in COOL mode — the AC is never disabled, only
// backed off — so a crash or a missing signal can never leave the house with no cooling on a
// warm night. Because the thermostat targets the house AVERAGE while the warm zone is
// upstairs, the setback aims the average at cap−strat so upstairs lands near the cap. Returns
// true while actively managing (the caller then skips the daytime pre-cool / free-cool logic).
func (m *Manager) handleNightFan(now time.Time, haveOut bool, outdoor float64, haveUp bool,
	upF float64, haveSet bool, setpoint float64, haveCur bool, current float64) bool {

	if !m.cfg.NightFan {
		return false
	}
	maxOut := orFloat(m.cfg.NightFanMaxOut, 80)
	capF := orFloat(m.cfg.NightFanCap, 78)
	strat := orFloat(m.cfg.NightFanStrat, 3)
	startH := orInt(m.cfg.NightFanStart, 22)
	endH := orInt(m.cfg.NightFanEnd, 6)

	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	h := now.In(loc).Hour()
	night := false
	if startH <= endH {
		night = h >= startH && h < endH
	} else { // window wraps past midnight
		night = h >= startH || h < endH
	}

	if !(night && haveOut && outdoor < maxOut && haveSet && haveUp) {
		m.stopNightFan()
		return false
	}

	// entry: remember the user's real setpoint so we can restore it
	if !m.nightFanning {
		m.nightFanning = true
		m.nfBaseSet = setpoint
		m.nfOverride = false
		m.nfLastCmd = 0
		slog.Info("climatebrain: night-fan test START", "outdoor", round1(outdoor),
			"upstairs", round1(upF), "cap", round1(capF), "base", round1(setpoint))
	}

	// user override: if the setpoint moved to something we didn't command, hand the house back
	// to the user for the rest of the window (fans off, don't fight).
	if !m.nfOverride && m.nfLastCmd != 0 && math.Abs(setpoint-m.nfLastCmd) > 0.4 {
		m.nfOverride = true
		slog.Info("climatebrain: night-fan override — user changed setpoint", "to", round1(setpoint))
	}
	if m.nfOverride {
		m.command(m.atticFanID(), false)
		m.command(m.boxFanID(), false)
		m.setNightFanStatus("override — user changed setpoint", capF, outdoor, upF)
		return true
	}

	// setback: aim the average at cap−strat so the warm upstairs zone lands near the cap.
	// Never below the user's base (don't cool more than normal); safety ceiling at 82°F.
	target := capF - strat
	if target < m.nfBaseSet {
		target = m.nfBaseSet
	}
	if target > 82 {
		target = 82
	}
	if math.Abs(setpoint-target) > 0.4 {
		m.setTemp(target)
		m.nfLastCmd = target
	} else if m.nfLastCmd == 0 {
		m.nfLastCmd = target
	}

	// fans: attic exhaust always (dumps the day's stored attic heat); box fan only when it's
	// genuinely cooler outside than the house, so it never pulls warm air in.
	m.command(m.atticFanID(), true)
	m.command(m.boxFanID(), haveCur && outdoor < current-0.5)

	m.setNightFanStatus(fmt.Sprintf("testing — AC setback to %.0f°F avg, fans on", target), capF, outdoor, upF)
	return true
}

func (m *Manager) stopNightFan() {
	if !m.nightFanning {
		return
	}
	m.nightFanning = false
	if !m.nfOverride {
		m.setTemp(m.nfBaseSet) // restore the user's setpoint
	}
	m.nfLastCmd = 0
	m.command(m.atticFanID(), false)
	m.command(m.boxFanID(), false)
	slog.Info("climatebrain: night-fan test END → restore", "base", round1(m.nfBaseSet))
	m.store.Set(entity.Entity{
		ID: "sensor.climatebrain_nightfan", Name: "Climate Brain Night-Fan", Domain: "sensor", State: "off",
		Attributes: map[string]any{"device": "climate-brain", "section": "climate"},
	})
}

func (m *Manager) setNightFanStatus(state string, capF, outdoor, upF float64) {
	m.store.Set(entity.Entity{
		ID: "sensor.climatebrain_nightfan", Name: "Climate Brain Night-Fan", Domain: "sensor", State: state,
		Attributes: map[string]any{"device": "climate-brain", "section": "climate",
			"cap": round1(capF), "outdoor": round1(outdoor), "upstairs": round1(upF),
			"attic_fan": m.switchState(m.atticFanID()), "box_fan": m.switchState(m.boxFanID())},
	})
}

// ── unified comfort-at-least-cost policy ─────────────────────────────────────
//
// ONE objective instead of a pile of modes: keep the WARM zone under the comfort ceiling (and
// the cool zone off the floor) using the CHEAPEST sufficient actuator, escalating circulate →
// attic fan → box fan (only when it's cooler out) → AC, with a solar pre-cool overlay. It steers
// off the OBSERVED warm-zone trend (feedback), so it self-corrects without a house thermal model.
// Runs in SHADOW (publishes what it WOULD do) until policy_actuate; meant to eventually replace
// free_cool / night_fan / precool with this single loop.

type tempSample struct {
	t time.Time
	v float64
}

type policyPlan struct {
	ac                string // "off" | "cool" | "precool"
	setpoint          float64
	circulate         bool
	atticFan          bool
	boxFan            bool
	warm, cool, trend float64
	reason            string
}

// warmTrend records the latest warm-zone reading and returns its recent slope (°F per 15 min).
func (m *Manager) warmTrend(now time.Time, warm float64) float64 {
	m.warmHist = append(m.warmHist, tempSample{now, warm})
	cut := now.Add(-20 * time.Minute)
	i := 0
	for i < len(m.warmHist) && m.warmHist[i].t.Before(cut) {
		i++
	}
	m.warmHist = m.warmHist[i:]
	if len(m.warmHist) < 3 {
		return 0
	}
	first, last := m.warmHist[0], m.warmHist[len(m.warmHist)-1]
	mins := last.t.Sub(first.t).Minutes()
	if mins < 1 {
		return 0
	}
	return (last.v - first.v) / mins * 15.0
}

func (m *Manager) evalPolicy(now time.Time, feelsUp, feelsDown float64, haveOut bool, outdoor, grid float64) policyPlan {
	high := orFloat(m.cfg.PolicyHigh, 78)
	low := orFloat(m.cfg.PolicyLow, 68)
	target := orFloat(m.cfg.PolicyTarget, 0)
	if target == 0 {
		if m.haveBase {
			target = m.baseSet
		} else {
			target = 73
		}
	}
	warm, cool := feelsUp, feelsDown
	if feelsDown > feelsUp {
		warm, cool = feelsDown, feelsUp
	}
	trend := m.warmTrend(now, warm)

	p := policyPlan{ac: "off", setpoint: high, warm: warm, cool: cool, trend: trend}
	p.circulate = math.Abs(feelsUp-feelsDown) > 2.5           // mix to cut stratification (cheap)
	p.atticFan = haveOut && warm > target && outdoor < warm-1 // dump attic heat when it helps
	p.boxFan = haveOut && warm > target && outdoor < cool-1   // pull outdoor air only when cooler out

	precool := grid < -400 && (!haveOut || outdoor < 92) && cool > low+2

	switch {
	case warm >= high:
		p.ac, p.setpoint = "cool", target
		p.reason = fmt.Sprintf("warm %.1f ≥ ceiling %.0f → AC to %.0f", warm, high, target)
	case warm > target+1 && trend > 0.3 && !p.boxFan && (!haveOut || outdoor >= high-2):
		p.ac, p.setpoint = "cool", target
		p.reason = fmt.Sprintf("warm %.1f rising %.1f/15m, outdoor %.0f too warm for fans → AC", warm, trend, outdoor)
	case precool:
		p.ac, p.setpoint = "precool", target-3
		p.reason = fmt.Sprintf("solar %.0fW surplus + efficient → pre-cool to %.0f (free power)", -grid, target-3)
	case warm > target:
		p.reason = fmt.Sprintf("warm %.1f, outdoor %.0f cooler → fans hold, AC off", warm, outdoor)
	default:
		p.reason = fmt.Sprintf("in band (warm %.1f ≤ %.0f) → coast", warm, target)
	}
	return p
}

func (m *Manager) runPolicy(now time.Time, feelsUp, feelsDown float64, haveOut bool, outdoor, grid float64) {
	p := m.evalPolicy(now, feelsUp, feelsDown, haveOut, outdoor, grid)
	prefix := ""
	if !m.cfg.PolicyActuate {
		prefix = "SHADOW · " // advisory only until policy_actuate
	}
	m.store.Set(entity.Entity{
		ID: "sensor.climatebrain_policy", Name: "Climate Brain Policy", Domain: "sensor",
		State: prefix + p.reason,
		Attributes: map[string]any{"device": "climate-brain", "section": "climate",
			"shadow": !m.cfg.PolicyActuate, "ac": p.ac, "setpoint": round1(p.setpoint),
			"circulate": p.circulate, "attic_fan": p.atticFan, "box_fan": p.boxFan,
			"warm": round1(p.warm), "cool": round1(p.cool), "trend_15m": round1(p.trend)},
	})
	// Phase C (policy_actuate): translate p into service.calls here, replacing the individual modes.
}

// ── solar pre-cool ─────────────────────────────────────────────────────────

func (m *Manager) handlePrecool(mode string, haveSet bool, setpoint float64, haveCur bool, current, grid float64, haveOut bool, outdoor float64) {
	if !m.cfg.PrecoolActuate {
		m.publishPrecoolAdvisory(mode, haveSet, setpoint, haveCur, current, grid, haveOut, outdoor)
		return
	}

	offset := orFloat(m.cfg.PrecoolOffset, 3)
	exportW := orFloat(m.cfg.PrecoolExportW, 400)
	exportOff := orFloat(m.cfg.PrecoolExportOffW, exportW/3)
	dwell := time.Duration(orFloat(m.cfg.PrecoolMinDwellMin, 20)) * time.Minute
	maxOut := orFloat(m.cfg.PrecoolMaxOut, 92)
	minF := orFloat(m.cfg.PrecoolMinF, 68)

	// Track the user's base setpoint + detect manual overrides so we never fight the user.
	if haveSet {
		switch {
		case !m.haveBase:
			m.baseSet, m.haveBase = setpoint, true
		case m.precooling && math.Abs(setpoint-m.lastCmd) > 0.4:
			// someone moved the setpoint while we were pre-cooling → adopt + stand down
			m.baseSet, m.precooling = setpoint, false
			slog.Info("climatebrain: precool override — user changed setpoint", "to", setpoint)
		case !m.precooling && math.Abs(setpoint-m.baseSet) > 0.4:
			m.baseSet = setpoint // user changed the base while we were idle
		}
	}

	target := m.baseSet - offset
	if target < minF {
		target = minF
	}

	// Hysteresis + min-dwell so a passing cloud can't flip-flop the setpoint (that short-cycles the
	// AC and wears the compressor). START needs strong export + headroom to bank; once engaged we
	// HOLD the target (no restore-and-drift on "banked") until export truly ends AND the dwell has
	// elapsed — one sustained pre-cool session per solar window, not dozens of setpoint bounces.
	startQualify := mode == "cool" && m.haveBase && grid < -exportW &&
		(!haveOut || outdoor <= maxOut) && haveCur && current > target+0.3
	holdQualify := mode == "cool" && m.haveBase && grid < -exportOff &&
		(!haveOut || outdoor <= maxOut)

	reason := ""
	switch {
	case mode != "cool":
		reason = "not cooling"
	case !m.haveBase:
		reason = "no setpoint yet"
	case !m.precooling && grid >= -exportW:
		reason = "no solar surplus"
	case m.precooling && grid >= -exportOff:
		reason = "solar surplus ended"
	case haveOut && outdoor > maxOut:
		reason = fmt.Sprintf("too hot out (%.0f°F)", outdoor)
	case !m.precooling && haveCur && current <= target+0.3:
		reason = "already at target"
	}

	if !m.precooling {
		if startQualify {
			m.setTemp(target)
			m.lastCmd = target
			m.precooling = true
			m.precoolSince = time.Now()
			slog.Info("climatebrain: pre-cooling", "target", target, "base", m.baseSet, "export_w", -grid)
		}
	} else {
		// Hold the session; stand down only after export has really ended AND the min-dwell passed.
		if !holdQualify && time.Since(m.precoolSince) >= dwell {
			m.setTemp(m.baseSet)
			m.lastCmd = m.baseSet
			m.precooling = false
			slog.Info("climatebrain: pre-cool end → restore", "base", m.baseSet, "reason", reason)
		} else if math.Abs(setpoint-target) > 0.4 {
			// base/target moved while holding -> re-assert once (not a bounce)
			m.setTemp(target)
			m.lastCmd = target
		}
	}

	state := "hold"
	if m.precooling {
		state = fmt.Sprintf("PRE-COOLING to %.0f°F (base %.0f, export %.0fW)", target, m.baseSet, -grid)
	} else if reason != "" {
		state = "hold (" + reason + ")"
	}
	m.store.Set(entity.Entity{
		ID: "sensor.climatebrain_precool", Name: "Climate Brain Pre-cool", Domain: "sensor", State: state,
		Attributes: map[string]any{"device": "climate-brain", "section": "climate",
			"precooling": m.precooling, "base_setpoint": round1(m.baseSet), "target": round1(target)},
	})
}

// handleHeatGuard protects upstairs comfort on a hot day. The on-device thermostat holds the
// AVERAGE of the upstairs + downstairs probes, so on a hot afternoon it will happily sit with
// upstairs at 77-78 and downstairs at 71 and call it satisfied. This says: it's hot out AND
// upstairs is over its cap → that's unacceptable → drop the setpoint enough to pull UPSTAIRS
// (not the average) down to the cap, and run the attic + box fans to break the stratification.
// Adaptive: the hotter it is outside, the tighter the cap. Returns true while it owns the setpoint.
//
// Safety: hard floor on the commanded setpoint (never freeze downstairs), bounded max drop,
// stands down the instant the user moves the setpoint, and the on-device 300s min-cycle is intact.
func (m *Manager) handleHeatGuard(haveOut bool, outdoor float64, haveUp bool, upF float64, haveSet bool, setpoint float64) bool {
	if !m.cfg.HeatGuard {
		return false
	}
	hotOut := orFloat(m.cfg.HeatGuardHotOut, 88) // "super hot" outside threshold °F
	upCap := orFloat(m.cfg.HeatGuardCap, 75)       // max upstairs °F allowed on a hot day
	floorF := orFloat(m.cfg.HeatGuardFloor, 70)  // never command the setpoint below this °F
	maxDrop := orFloat(m.cfg.HeatGuardMaxDrop, 4)

	// Track the user's base setpoint + honor manual overrides (same discipline as pre-cool).
	if haveSet {
		switch {
		case !m.haveBase:
			m.baseSet, m.haveBase = setpoint, true
		case m.heatGuarding && math.Abs(setpoint-m.hgLastCmd) > 0.4:
			m.baseSet, m.heatGuarding = setpoint, false // user moved it → adopt + stand down
			slog.Info("climatebrain: heat-guard override — user changed setpoint", "to", setpoint)
		case !m.heatGuarding && math.Abs(setpoint-m.baseSet) > 0.4:
			m.baseSet = setpoint
		}
	}

	// Adaptive upCap: tighten by up to 1°F as outdoor climbs past the threshold toward ~98.
	if haveOut && outdoor > hotOut {
		upCap -= math.Min(1, (outdoor-hotOut)/10)
	}

	engaged := haveOut && haveUp && m.haveBase && outdoor >= hotOut
	over := upF - upCap

	target := m.baseSet
	reason := ""
	switch {
	case !engaged:
		reason = fmt.Sprintf("not hot out (%.0f<%.0f)", outdoor, hotOut)
	case over <= 0:
		reason = fmt.Sprintf("upstairs %.1f ≤ upCap %.1f", upF, upCap)
	default:
		// Push the setpoint below base by the upstairs overshoot (avg-based thermostat tracks
		// ~1:1 at steady state), bounded by max-drop and the hard floor.
		target = m.baseSet - math.Min(over, maxDrop)
		if target < floorF {
			target = floorF
		}
	}

	if engaged && over > 0 {
		// attack the stratification directly with the fans
		m.command(m.atticFanID(), true)
		m.command(m.boxFanID(), true)
		m.arm = "" // a forced-on fan voids any attic A/B arm in progress
		if !m.heatGuarding || math.Abs(setpoint-target) > 0.4 {
			m.setTemp(target)
			m.hgLastCmd = target
			m.heatGuarding = true
			slog.Info("climatebrain: HEAT-GUARD cooling upstairs", "upstairs", round1(upF),
				"upCap", round1(upCap), "outdoor", round1(outdoor), "target", round1(target), "base", round1(m.baseSet))
		}
	} else if m.heatGuarding {
		// upstairs back under the upCap (with hysteresis) or no longer hot → restore the base setpoint
		if !engaged || upF <= upCap-0.5 {
			m.setTemp(m.baseSet)
			m.hgLastCmd = m.baseSet
			m.heatGuarding = false
			m.command(m.boxFanID(), false) // release the box fan; leave attic to its own logic
			slog.Info("climatebrain: heat-guard release → restore", "base", round1(m.baseSet), "reason", reason)
		}
	}

	state := "hold"
	if m.heatGuarding {
		state = fmt.Sprintf("GUARDING upstairs %.1f→%.0f (setpoint %.0f, out %.0f)", upF, upCap, target, outdoor)
	} else if reason != "" {
		state = "hold (" + reason + ")"
	}
	m.store.Set(entity.Entity{
		ID: "sensor.climatebrain_heatguard", Name: "Climate Brain Heat-Guard", Domain: "sensor", State: state,
		Attributes: map[string]any{"device": "climate-brain", "section": "climate",
			"guarding": m.heatGuarding, "upstairs": round1(upF), "upCap": round1(upCap),
			"outdoor": round1(outdoor), "target": round1(target), "base_setpoint": round1(m.baseSet)},
	})
	return m.heatGuarding
}

// publishPrecoolAdvisory = the v1 recommend-only path (when precool_actuate is off).
func (m *Manager) publishPrecoolAdvisory(mode string, haveSet bool, setpoint float64, haveCur bool, current, grid float64, haveOut bool, outdoor float64) {
	advice, reason := "hold", ""
	switch {
	case mode != "cool":
		reason = "not in cool mode"
	case grid > -orFloat(m.cfg.PrecoolExportW, 400):
		reason = "no solar surplus"
	case haveOut && outdoor > orFloat(m.cfg.PrecoolMaxOut, 92):
		reason = "too hot out — AC inefficient"
	case haveSet && haveCur && current <= setpoint-3:
		reason = "already pre-cooled"
	default:
		if haveSet {
			advice = fmt.Sprintf("RECOMMEND pre-cool to %.0f°F", setpoint-3)
		} else {
			advice = "RECOMMEND pre-cool"
		}
		if grid < 0 {
			reason = fmt.Sprintf("exporting %.0fW", -grid)
		}
	}
	state := advice
	if reason != "" {
		state = advice + " (" + reason + ")"
	}
	m.store.Set(entity.Entity{
		ID: "sensor.climatebrain_precool", Name: "Climate Brain Pre-cool", Domain: "sensor", State: state,
		Attributes: map[string]any{"device": "climate-brain", "section": "climate", "advisory": true},
	})
}

func (m *Manager) setTemp(v float64) {
	m.bus.Publish("service.call", map[string]any{
		"service": "climate.set_temperature", "entity": "climate.house",
		"data":   map[string]any{"temperature": round1(v)},
		"source": "brain", // so the thermostat/override logic never mistakes our command for the user's
	})
}

// logOverride records a manual setpoint override + its context, as a labeled comfort signal the
// brain can later analyze ("the user set 75 when feels-up was X, outdoor Y, at hour H → learn why").
func (m *Manager) logOverride(now time.Time, source string, setpoint, feelsUp, feelsDown float64, haveOut bool, outdoor float64) {
	rec := map[string]any{
		"ts": now.Format(time.RFC3339), "source": source, "setpoint": round1(setpoint),
		"feels_up": round1(feelsUp), "feels_down": round1(feelsDown), "hour": now.Hour(),
	}
	if haveOut {
		rec["outdoor"] = round1(outdoor)
	}
	f, err := os.OpenFile(overrideFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(rec); err == nil {
		f.Write(append(b, '\n'))
	}
}

// ── comfort model ────────────────────────────────────────────────────────────

// learnComfort reads the comfort-feedback log and derives a comfortable setpoint: the median
// setpoint recorded during "good" taps, nudged by "hot"/"cold" evidence. Advisory only — the
// brain never auto-WARMS past what the user set; it just surfaces + floors pre-cool.
func (m *Manager) learnComfort() {
	f, err := os.Open(comfortFile)
	if err != nil {
		return
	}
	defer f.Close()
	var good, hot, cold []float64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var rec struct {
			Rating  string         `json:"rating"`
			Context map[string]any `json:"context"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		sp, ok := toF(rec.Context["setpoint"])
		if !ok {
			continue
		}
		switch rec.Rating {
		case "good":
			good = append(good, sp)
		case "hot":
			hot = append(hot, sp)
		case "cold":
			cold = append(cold, sp)
		}
	}
	set, n := 0.0, 0
	switch {
	case len(good) > 0:
		set, n = median(good), len(good)
	case len(hot) > 0: // never felt "good", but "hot" at these setpoints → comfortable is below
		set, n = median(hot)-1, len(hot)
	case len(cold) > 0:
		set, n = median(cold)+1, len(cold)
	}
	if n == 0 {
		return
	}
	m.comfortSet, m.comfortN = set, n
	m.store.Set(entity.Entity{
		ID: "sensor.climatebrain_comfort", Name: "Learned Comfortable Setpoint", Domain: "sensor",
		State: strconv.FormatFloat(round1(set), 'f', 1, 64),
		Attributes: map[string]any{"device": "climate-brain", "section": "climate",
			"unit_of_measurement": "°F", "good_taps": len(good), "hot_taps": len(hot), "cold_taps": len(cold)},
	})
}

// ── snapshot + observability ─────────────────────────────────────────────────

type snapshot struct {
	haveUp, haveDown, haveAlt, haveDelta, haveOut                                      bool
	upF, downF, altF, upRH, downRH, feelsUp, feelsDown, delta, outdoor, pv, grid, batt float64
	upRHn, downRHn                                                                     int
	hvac, atticFan, boxFan                                                             string
}

func (m *Manager) logSnapshot(now time.Time, s snapshot) {
	rec := map[string]any{"time": now.Format(time.RFC3339), "hvac": s.hvac, "attic_fan": s.atticFan,
		"box_fan": s.boxFan, "pv": s.pv, "grid": s.grid, "batt_soc": s.batt, "exp_arm": m.arm,
		"attic_verdict": m.atticVerdict, "precooling": m.precooling}
	if s.haveUp {
		rec["upstairs"] = round1(s.upF)
		rec["feels_up"] = round1(s.feelsUp)
	}
	if s.haveDown {
		rec["downstairs"] = round1(s.downF)
		rec["feels_down"] = round1(s.feelsDown)
	}
	if s.haveAlt {
		rec["upstairs_alt"] = round1(s.altF)
	}
	if s.upRHn > 0 {
		rec["up_rh"] = round1(s.upRH)
	}
	if s.downRHn > 0 {
		rec["down_rh"] = round1(s.downRH)
	}
	if s.haveDelta {
		rec["delta"] = round1(s.delta)
	}
	if s.haveOut {
		rec["outdoor"] = s.outdoor
	}
	if m.haveBase {
		rec["base_setpoint"] = round1(m.baseSet)
	}
	if line, err := json.Marshal(rec); err == nil {
		if f, ferr := os.OpenFile("/data/climate-brain.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); ferr == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
}

func (m *Manager) publish(now time.Time, haveDelta bool, delta float64, haveOut bool, outdoor, upF float64, qualify bool,
	haveUp bool, feelsUp, upRH float64, upRHn int, haveDown bool, feelsDown, downRH float64, downRHn int) {
	set := func(id, name, state string, attr map[string]any) {
		if attr == nil {
			attr = map[string]any{}
		}
		attr["device"], attr["section"] = "climate-brain", "climate"
		m.store.Set(entity.Entity{ID: id, Name: name, Domain: "sensor", State: state, Attributes: attr})
	}

	status := "observing"
	switch {
	case m.holdStatus != "":
		status = m.holdStatus // manual-override hold wins
	case m.freeCooling:
		status = "FREE-COOLING — fans pulling cool air in, AC idle"
	case m.arm == "run":
		status = "attic fan ON (learned: it helps)"
	case m.arm == "on" || m.arm == "off":
		status = fmt.Sprintf("attic A/B: fan %s (%dm)", m.arm, int(now.Sub(m.armStart).Minutes()))
	case m.precooling:
		status = "pre-cooling on solar"
	case m.cfg.AtticExperiment && m.atticVerdict == "" && !qualify:
		status = "attic A/B: waiting for hot+sun"
	}
	set("sensor.climatebrain_status", "Climate Brain Status", status, nil)

	if haveOut {
		set("sensor.climatebrain_load", "Climate Load (outdoor−upstairs)",
			strconv.FormatFloat(round1(outdoor-upF), 'f', 1, 64), map[string]any{"unit_of_measurement": "°F"})
	}

	result := "gathering…"
	if m.atticVerdict != "" {
		result = fmt.Sprintf("verdict: %s — AC duty fan ON %s vs OFF %s (n=%d/%d)",
			m.atticVerdict, m.avg(m.onDutySum, m.onN), m.avg(m.offDutySum, m.offN), m.onN, m.offN)
	} else if m.onN > 0 && m.offN > 0 {
		result = fmt.Sprintf("AC duty — fan ON %s vs OFF %s (n=%d/%d)",
			m.avg(m.onDutySum, m.onN), m.avg(m.offDutySum, m.offN), m.onN, m.offN)
	}
	set("sensor.climatebrain_atticfan_effect", "Attic Fan Effect", result,
		map[string]any{"on_samples": m.onN, "off_samples": m.offN, "verdict": m.atticVerdict})

	// feels-like per floor (humidity-aware comfort)
	if haveUp && upRHn > 0 {
		set("sensor.climatebrain_feels_upstairs", "Feels Like Upstairs",
			strconv.FormatFloat(round1(feelsUp), 'f', 1, 64),
			map[string]any{"unit_of_measurement": "°F", "humidity": round1(upRH)})
	}
	if haveDown && downRHn > 0 {
		set("sensor.climatebrain_feels_downstairs", "Feels Like Downstairs",
			strconv.FormatFloat(round1(feelsDown), 'f', 1, 64),
			map[string]any{"unit_of_measurement": "°F", "humidity": round1(downRH)})
	}
}

// ── persistence ──────────────────────────────────────────────────────────────

func (m *Manager) loadState() {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return
	}
	var ps persistState
	if json.Unmarshal(b, &ps) != nil {
		return
	}
	m.onDutySum, m.offDutySum, m.onN, m.offN = ps.OnDutySum, ps.OffDutySum, ps.OnN, ps.OffN
	m.atticVerdict, m.baseSet, m.haveBase = ps.AtticVerdict, ps.BaseSet, ps.HaveBase
	m.precooling, m.lastCmd = ps.Precooling, ps.LastCmd
	m.nightFanning, m.nfBaseSet = ps.NightFanning, ps.NfBaseSet
	m.checkAtticVerdict() // re-evaluate a verdict from persisted A/B data on startup
}

func (m *Manager) saveState() {
	ps := persistState{OnDutySum: m.onDutySum, OffDutySum: m.offDutySum, OnN: m.onN, OffN: m.offN,
		AtticVerdict: m.atticVerdict, BaseSet: m.baseSet, HaveBase: m.haveBase,
		Precooling: m.precooling, LastCmd: m.lastCmd,
		NightFanning: m.nightFanning, NfBaseSet: m.nfBaseSet}
	if b, err := json.Marshal(ps); err == nil {
		os.WriteFile(stateFile, b, 0644)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (m *Manager) num(id string) (float64, bool) {
	e, ok := m.store.Get(id)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(e.State, 64)
	return f, err == nil
}

// avgValid returns the mean of the parseable states among the given entity ids, and the count.
func (m *Manager) avgValid(ids []string) (float64, int) {
	sum, n := 0.0, 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		if v, ok := m.num(id); ok {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

func (m *Manager) atticFanID() string {
	if m.cfg.AtticFan != "" {
		return m.cfg.AtticFan
	}
	return "switch.intgarage_attic_relay"
}
func (m *Manager) boxFanID() string {
	if m.cfg.BoxFan != "" {
		return m.cfg.BoxFan
	}
	return "switch.attached_garage_attic_fan_relay"
}

func (m *Manager) switchState(id string) string {
	if e, ok := m.store.Get(id); ok {
		return e.State
	}
	return ""
}

func (m *Manager) command(entityID string, on bool) {
	// avoid redundant commands (don't spam the bus if it's already in the wanted state)
	cur := m.switchState(entityID)
	if (on && cur == "on") || (!on && cur == "off") {
		return
	}
	svc := "switch.turn_off"
	if on {
		svc = "switch.turn_on"
	}
	m.bus.Publish("service.call", map[string]any{"service": svc, "entity": entityID, "data": nil})
	slog.Info("climatebrain: command", "entity", entityID, "on", on)
}

func (m *Manager) avg(sum float64, n int) string {
	if n == 0 {
		return "—"
	}
	return pct(sum / float64(n))
}

// heatIndex returns a "feels-like" °F from dry-bulb T (°F) and RH (%). Uses the NWS Rothfusz
// regression at/above 80°F; below that (typical indoor cooling range) it applies a light
// mugginess offset (humid feels warmer, dry feels cooler) so comfort tracks RH, not just temp.
func heatIndex(T, rh float64) float64 {
	if T >= 80 {
		hi := -42.379 + 2.04901523*T + 10.14333127*rh - 0.22475541*T*rh -
			0.00683783*T*T - 0.05481717*rh*rh + 0.00122874*T*T*rh +
			0.00085282*T*rh*rh - 0.00000199*T*T*rh*rh
		if hi < T {
			return T
		}
		return hi
	}
	off := (rh - 50) / 10 * 0.6
	if off > 3 {
		off = 3
	} else if off < -1.5 {
		off = -1.5
	}
	return T + off
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func toF(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func pct(f float64) string     { return fmt.Sprintf("%.0f%%", f*100) }
func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
func orFloat(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}
