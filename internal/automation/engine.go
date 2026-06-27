package automation

import (
	"context"
	"log/slog"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

type Engine struct {
	automations []config.AutomationConfig
	store       *entity.Store
	bus         *bus.Bus
}

func NewEngine(automations []config.AutomationConfig, store *entity.Store, b *bus.Bus) *Engine {
	return &Engine{
		automations: automations,
		store:       store,
		bus:         b,
	}
}

func (e *Engine) Run(ctx context.Context) {
	// Subscribe to state changes to evaluate state_change triggers.
	e.bus.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
		payload, ok := ev.Payload.(entity.StateChangedPayload)
		if !ok {
			return
		}
		for _, a := range e.automations {
			if a.Trigger.Type != "state_change" {
				continue
			}
			if a.Trigger.Entity != payload.Entity.ID {
				continue
			}
			if a.Trigger.To != "" && a.Trigger.To != payload.Entity.State {
				continue
			}
			if a.Trigger.From != "" && a.Trigger.From != payload.OldState {
				continue
			}
			if !e.checkCondition(a.Condition) {
				continue
			}
			slog.Info("automation triggered", "name", a.Name, "entity", payload.Entity.ID, "state", payload.Entity.State)
			go e.runActions(ctx, a.Action)
		}
	})

	<-ctx.Done()
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
