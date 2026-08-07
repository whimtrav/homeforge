package automation

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

type Engine struct {
	automations []config.AutomationConfig
	store       *entity.Store
	bus         *bus.Bus
	state       *StateStore // per-automation enabled/disabled overrides (nil = all enabled)
	lat, lon    float64     // site location for sun (sunrise/sunset) triggers
}

func NewEngine(automations []config.AutomationConfig, store *entity.Store, b *bus.Bus, state *StateStore, lat, lon float64) *Engine {
	return &Engine{
		automations: automations,
		store:       store,
		bus:         b,
		state:       state,
		lat:         lat,
		lon:         lon,
	}
}

func (e *Engine) Run(ctx context.Context) {
	// Subscribe to state changes to evaluate state_change and numeric triggers.
	e.bus.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
		payload, ok := ev.Payload.(entity.StateChangedPayload)
		if !ok {
			return
		}
		for _, a := range e.automations {
			if a.Trigger.Entity != payload.Entity.ID {
				continue
			}
			if !e.state.Enabled(a.Name) { // user-disabled via the UI
				continue
			}
			if !triggerFires(a.Trigger, payload) {
				continue
			}
			if !e.checkCondition(a.Condition) {
				continue
			}
			slog.Info("automation triggered", "name", a.Name, "entity", payload.Entity.ID, "state", payload.Entity.State)
			go e.runActions(ctx, a.Action)
		}
	})

	// Sun (sunrise/sunset) triggers run on a time ticker, not on state changes.
	go e.runSunTriggers(ctx)

	<-ctx.Done()
}

// runSunTriggers fires automations whose trigger.type == "sun" at the day's
// sunrise/sunset (± offset minutes), once per day each. A 30s ticker recomputes
// the sun times and fires on the first tick at/after the target; a target missed
// by >10 min (e.g. HF started well after it) is marked done, not fired stale.
func (e *Engine) runSunTriggers(ctx context.Context) {
	hasSun := false
	for _, a := range e.automations {
		if a.Trigger.Type == "sun" {
			hasSun = true
			break
		}
	}
	if !hasSun {
		return
	}
	fired := map[string]string{} // automation name -> yyyy-mm-dd last fired
	tk := time.NewTicker(30 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			now := time.Now()
			today := now.Format("2006-01-02")
			sunrise, sunset := SunTimes(e.lat, e.lon, now)
			for _, a := range e.automations {
				if a.Trigger.Type != "sun" || !e.state.Enabled(a.Name) {
					continue
				}
				var target time.Time
				switch a.Trigger.Event {
				case "sunset":
					target = sunset
				case "sunrise":
					target = sunrise
				default:
					continue
				}
				target = target.Add(time.Duration(a.Trigger.Offset) * time.Minute)
				if fired[a.Name] == today || now.Before(target) {
					continue
				}
				if now.Sub(target) > 10*time.Minute { // stale (e.g. startup long after) — skip
					fired[a.Name] = today
					continue
				}
				fired[a.Name] = today
				if !e.checkCondition(a.Condition) {
					continue
				}
				slog.Info("automation triggered (sun)", "name", a.Name, "event", a.Trigger.Event, "target", target.Format("15:04"))
				go e.runActions(ctx, a.Action)
			}
		}
	}
}

// triggerFires decides whether an incoming state change matches a trigger.
//   - state_change: optional to/from string match (empty = any).
//   - numeric: crosses a threshold. `above` fires on the sample where the value
//     goes from <=X to >X; `below` fires on >=X to <X. Requires a valid prior
//     value so it fires only on the transition, never repeatedly while past it.
func triggerFires(t config.TriggerConfig, p entity.StateChangedPayload) bool {
	switch t.Type {
	case "state_change":
		if t.To != "" && t.To != p.Entity.State {
			return false
		}
		if t.From != "" && t.From != p.OldState {
			return false
		}
		return true
	case "numeric":
		newV, err1 := strconv.ParseFloat(p.Entity.State, 64)
		oldV, err2 := strconv.ParseFloat(p.OldState, 64)
		if err1 != nil || err2 != nil {
			return false // need both values to detect a crossing
		}
		if t.Above != nil {
			return oldV <= *t.Above && newV > *t.Above
		}
		if t.Below != nil {
			return oldV >= *t.Below && newV < *t.Below
		}
		return false
	}
	return false
}

func (e *Engine) checkCondition(c *config.ConditionConfig) bool {
	if c == nil {
		return true
	}
	switch c.Type {
	case "state":
		ent, ok := e.store.Get(c.Entity)
		if !ok {
			return false
		}
		return ent.State == c.State
	case "numeric":
		ent, ok := e.store.Get(c.Entity)
		if !ok {
			return false
		}
		v, err := strconv.ParseFloat(ent.State, 64)
		if err != nil {
			return false
		}
		if c.Above != nil && v <= *c.Above {
			return false
		}
		if c.Below != nil && v >= *c.Below {
			return false
		}
		return true
	case "and":
		for _, sub := range c.Conditions {
			if !e.checkCondition(sub) {
				return false
			}
		}
		return true
	case "or":
		if len(c.Conditions) == 0 {
			return true
		}
		for _, sub := range c.Conditions {
			if e.checkCondition(sub) {
				return true
			}
		}
		return false
	case "time_range":
		now := time.Now().Format("15:04")
		return now >= c.After && now <= c.Before
	}
	return true
}

func (e *Engine) runActions(ctx context.Context, actions []config.ActionConfig) {
	for _, action := range actions {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if action.Wait != "" {
			d, err := time.ParseDuration(action.Wait)
			if err == nil {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return
				}
				if action.Condition != nil && !e.checkCondition(action.Condition) {
					slog.Info("automation: sequence aborted (post-wait condition false)")
					return
				}
				continue
			}
		}

		e.callService(action)
	}
}

func (e *Engine) callService(action config.ActionConfig) {
	slog.Info("automation: call service",
		"service", action.Service,
		"entity", action.Entity,
		"data", action.Data,
	)
	e.bus.Publish("service.call", map[string]any{
		"service": action.Service,
		"entity":  action.Entity,
		"data":    action.Data,
	})
}
