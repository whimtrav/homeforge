package api

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sync"
	"time"
)

// Billing-cycle energy tracker. NorthWestern's meter-read date wanders (bills show 29-32 day
// cycles), so we run a 30-day ESTIMATE from a persisted anchor and let the actual bill rectify
// it: when a bill arrives the user enters its read date + the "generated exceeds used by N kWh"
// credit-bank number, which re-anchors the cycle exactly and logs the surplus trend. Totals come
// from the cumulative solar counters' history (reset-robust positive-delta sum, History.CycleTotal).

const energyCycleFile = "/data/energy-cycle.json"
const energyCycleDefaultStart = "2026-07-24" // last known meter read
const energyCycleTZ = "America/Denver"

type billEntry struct {
	ClosedOn string  `json:"closed_on"` // date the bill was logged in HF
	ReadTo   string  `json:"read_to"`   // meter read date = start of the NEXT cycle
	BankKWh  float64 `json:"bank_kwh"`  // "generated exceeds used by" credit surplus
	Import   float64 `json:"import"`    // grid import over the closed cycle
	Export   float64 `json:"export"`    // grid export over the closed cycle
}

type energyCycleState struct {
	CycleStart string      `json:"cycle_start"`
	Bills      []billEntry `json:"bills"`
}

var energyCycleMu sync.Mutex

func loadEnergyCycle() energyCycleState {
	st := energyCycleState{CycleStart: energyCycleDefaultStart}
	if b, err := os.ReadFile(energyCycleFile); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.CycleStart == "" {
		st.CycleStart = energyCycleDefaultStart
	}
	return st
}

func saveEnergyCycle(st energyCycleState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := energyCycleFile + ".tmp"
	if os.WriteFile(tmp, b, 0644) == nil {
		os.Rename(tmp, energyCycleFile)
	}
}

func energyLoc() *time.Location {
	loc, err := time.LoadLocation(energyCycleTZ)
	if err != nil {
		return time.UTC
	}
	return loc
}

// GET /api/energy/cycle — current cycle totals + credit-bank trend + whether a bill is due.
func (s *Server) handleEnergyCycle(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.Error(w, "history disabled", http.StatusNotImplemented)
		return
	}
	energyCycleMu.Lock()
	st := loadEnergyCycle()
	energyCycleMu.Unlock()

	loc := energyLoc()
	start, err := time.ParseInLocation("2006-01-02", st.CycleStart, loc)
	if err != nil {
		start = time.Now().AddDate(0, 0, -30)
	}
	end := time.Now()
	calc := func(eid string) float64 {
		v, err := s.history.CycleTotal(r.Context(), eid, start, end)
		if err != nil {
			slog.Error("api: energy cycle failed", "entity", eid, "err", err)
			return 0
		}
		return math.Round(v*10) / 10
	}
	imp := calc("sensor.solar_grid_energy_in")
	exp := calc("sensor.solar_grid_energy_out")
	gen := calc("sensor.solar_pv_energy")
	use := calc("sensor.solar_load_energy")
	days := int(end.Sub(start).Hours()/24) + 1

	// "Made vs used" net = generation − consumption over a window. + = made more than used (GREEN),
	// − = used more than made (RED). The card's red/green tiles color on THIS (the user's rule),
	// not grid import/export — a battery day can import overnight yet still out-produce the house.
	madeNet := func(a, b time.Time) float64 {
		g, e1 := s.history.CycleTotal(r.Context(), "sensor.solar_pv_energy", a, b)
		u, e2 := s.history.CycleTotal(r.Context(), "sensor.solar_load_energy", a, b)
		if e1 != nil || e2 != nil {
			return 0
		}
		return math.Round((g-u)*10) / 10
	}
	nowLoc := end.In(loc)
	todayStart := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(), 0, 0, 0, 0, loc)
	yestStart := todayStart.AddDate(0, 0, -1)
	todayMadeNet := madeNet(todayStart, end)
	yesterdayMadeNet := madeNet(yestStart, todayStart)
	monthMadeNet := math.Round((gen-use)*10) / 10

	var bank, bankPrev float64
	if n := len(st.Bills); n > 0 {
		bank = st.Bills[n-1].BankKWh
		if n > 1 {
			bankPrev = st.Bills[n-2].BankKWh
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"cycle_start":        st.CycleStart,
		"day":                days,
		"expected_days":      30,
		"grid_import":        imp,
		"grid_export":        exp,
		"grid_net":           math.Round((imp-exp)*10) / 10, // + = net import (drawing the bank), - = banking
		"generation":         gen,
		"consumption":        use,
		"made_net":           monthMadeNet,     // cycle generation − consumption; GREEN if ≥0 (made ≥ used)
		"today_made_net":     todayMadeNet,     // today so far (local day), made − used
		"yesterday_made_net": yesterdayMadeNet, // full prior local day, made − used
		"needs_bill":         days >= 30,       // ~a cycle old → the bill should be here → prompt to log it
		"bank_kwh":           bank,
		"bank_delta":         math.Round((bank-bankPrev)*10) / 10,
		"bills":              st.Bills,
	})
}

// POST /api/energy/cycle/rectify — the bill arrived. Body: {read_to:"2026-08-24", bank_kwh:2020}.
// Closes the running cycle (logs its import/export + the bank number) and re-anchors to read_to.
func (s *Server) handleEnergyRectify(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.Error(w, "history disabled", http.StatusNotImplemented)
		return
	}
	var body struct {
		ReadTo  string  `json:"read_to"`
		BankKWh float64 `json:"bank_kwh"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ReadTo == "" {
		http.Error(w, "need {read_to, bank_kwh}", http.StatusBadRequest)
		return
	}
	loc := energyLoc()
	readTo, err := time.ParseInLocation("2006-01-02", body.ReadTo, loc)
	if err != nil {
		http.Error(w, "read_to must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}

	energyCycleMu.Lock()
	st := loadEnergyCycle()
	start, _ := time.ParseInLocation("2006-01-02", st.CycleStart, loc)
	imp, _ := s.history.CycleTotal(r.Context(), "sensor.solar_grid_energy_in", start, readTo)
	exp, _ := s.history.CycleTotal(r.Context(), "sensor.solar_grid_energy_out", start, readTo)
	st.Bills = append(st.Bills, billEntry{
		ClosedOn: time.Now().In(loc).Format("2006-01-02"),
		ReadTo:   body.ReadTo,
		BankKWh:  body.BankKWh,
		Import:   math.Round(imp*10) / 10,
		Export:   math.Round(exp*10) / 10,
	})
	st.CycleStart = body.ReadTo // the new cycle begins at the read date
	saveEnergyCycle(st)
	energyCycleMu.Unlock()

	if s.alexaCfg.Enabled {
		s.setBillReminder(false) // logging the bill clears the Alexa reminder sensor
	}

	slog.Info("api: energy cycle rectified", "read_to", body.ReadTo, "bank_kwh", body.BankKWh)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "cycle_start": st.CycleStart, "bills": len(st.Bills)})
}
