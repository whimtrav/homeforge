package main

import (
	"context"
	"sync"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/whimtrav/homeforge/internal/api"
	"github.com/whimtrav/homeforge/internal/automation"
	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
	"github.com/whimtrav/homeforge/internal/groups"
	"github.com/whimtrav/homeforge/internal/history"
	"github.com/whimtrav/homeforge/internal/integrations/esphome"
	"github.com/whimtrav/homeforge/internal/integrations/liquidfw"
	"github.com/whimtrav/homeforge/internal/integrations/sonoff"
	"github.com/whimtrav/homeforge/internal/climatebrain"
	"github.com/whimtrav/homeforge/internal/integrations/disagg"
	"github.com/whimtrav/homeforge/internal/integrations/rachio"
	"github.com/whimtrav/homeforge/internal/integrations/tigo"
	"github.com/whimtrav/homeforge/internal/integrations/weather"
	"github.com/whimtrav/homeforge/internal/integrations/wiz"
	"github.com/whimtrav/homeforge/internal/integrations/wled"
	"github.com/whimtrav/homeforge/internal/mqtt"
	"github.com/whimtrav/homeforge/internal/thermostat"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// CLI subcommands for LiquidFW key management
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "liquidfw-keygen":
			keyFile := "liquidfw.key"
			if len(os.Args) > 2 {
				keyFile = os.Args[2]
			}
			pub, err := liquidfw.Pubkey(keyFile)
			if err != nil {
				slog.Error("keygen failed", "err", err)
				os.Exit(1)
			}
			fmt.Printf("Keypair saved to %s\n", keyFile)
			fmt.Printf("Public key: %s\n\n", pub)
			fmt.Printf("Add to device config.json:\n  \"homeforge_pub\": \"%s\"\n", pub)
			fmt.Printf("Or run:  homeforge liquidfw-provision <device-ip> [key-file]\n")
			return
		case "liquidfw-provision":
			if len(os.Args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: homeforge liquidfw-provision <device-ip> [key-file]")
				os.Exit(1)
			}
			deviceIP := os.Args[2]
			keyFile := "liquidfw.key"
			if len(os.Args) > 3 {
				keyFile = os.Args[3]
			}
			if err := liquidfw.Provision(keyFile, deviceIP); err != nil {
				slog.Error("provision failed", "err", err)
				os.Exit(1)
			}
			fmt.Printf("Provisioned %s — device will reboot in encrypted mode\n", deviceIP)
			return
		case "liquidfw-pubkey":
			keyFile := "liquidfw.key"
			if len(os.Args) > 2 {
				keyFile = os.Args[2]
			}
			pub, err := liquidfw.Pubkey(keyFile)
			if err != nil {
				slog.Error("pubkey failed", "err", err)
				os.Exit(1)
			}
			fmt.Println(pub)
			return
		case "liquidfw-cmd":
			// usage: homeforge liquidfw-cmd <device-name> <json> [key-file] [registry-file]
			if len(os.Args) < 4 {
				fmt.Fprintln(os.Stderr, "usage: homeforge liquidfw-cmd <device-name> <json-payload> [key-file] [registry-file]")
				fmt.Fprintln(os.Stderr, `example: homeforge liquidfw-cmd liquidfw-test '{"led":true}'`)
				os.Exit(1)
			}
			deviceName := os.Args[2]
			payload := []byte(os.Args[3])
			keyFile := "liquidfw.key"
			registryFile := "liquidfw-devices.json"
			if len(os.Args) > 4 {
				keyFile = os.Args[4]
			}
			if len(os.Args) > 5 {
				registryFile = os.Args[5]
			}
			if err := liquidfw.SendCmd(keyFile, registryFile, deviceName, payload); err != nil {
				slog.Error("cmd failed", "err", err)
				os.Exit(1)
			}
			fmt.Println("ok")
			return
		}
	}

	cfgPath := "config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	eventBus := bus.New()
	store := entity.NewStore(eventBus, "/data/entity-snapshot.json")

	mqttSrv, err := mqtt.NewServer(cfg.MQTT, store, eventBus)
	if err != nil {
		slog.Error("failed to start MQTT server", "err", err)
		os.Exit(1)
	}

	// Per-automation enable/disable overrides, persisted separately from config.yaml
	// (so UI toggles never rewrite the hand-commented config) and shared across reloads.
	autoState := automation.NewStateStore("/data/automation-state.json")

	// Restartable automation engine.
	var engineMu sync.Mutex
	var engineCancel context.CancelFunc
	startEngine := func(automations []config.AutomationConfig) {
		engineCtx, cancel := context.WithCancel(ctx)
		engineCancel = cancel
		go automation.NewEngine(automations, store, eventBus, autoState, cfg.Integrations.Weather.Lat, cfg.Integrations.Weather.Lon).Run(engineCtx)
	}
	startEngine(cfg.Automations)

	apiSrv := api.NewServer(cfg.API, store, eventBus)
	apiSrv.SetAuth(cfg.Auth)
	apiSrv.SetCameras(cfg.Cameras)
	apiSrv.SetAutomations(cfg.Automations)
	apiSrv.SetAssistant(cfg.Assistant)
	apiSrv.SetAutomationState(autoState)
	apiSrv.SetAlexa(cfg.Integrations.Alexa)
	apiSrv.SetReload(func() error {
		newCfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		engineMu.Lock()
		defer engineMu.Unlock()
		if engineCancel != nil {
			engineCancel()
		}
		startEngine(newCfg.Automations)
		apiSrv.SetAutomations(newCfg.Automations)
		return nil
	})

	// State-history recorder (TimescaleDB sidecar). Optional — a failure here
	// must never take down the hub, so we log and continue on error.
	if cfg.History.Enabled {
		hist, err := history.Open(ctx, history.Config{
			Enabled:       cfg.History.Enabled,
			DSN:           cfg.History.DSN,
			RetentionRaw:  cfg.History.RetentionRaw,
			Retention1m:   cfg.History.Retention1m,
			Retention1h:   cfg.History.Retention1h,
			CompressAfter: cfg.History.CompressAfter,
		})
		if err != nil {
			slog.Error("history: disabled (open failed)", "err", err)
		} else {
			eventBus.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
				if p, ok := ev.Payload.(entity.StateChangedPayload); ok {
					hist.Record(p.Entity)
				}
			})
			apiSrv.SetHistory(hist)
		}
	}

	slog.Info("HomeForge starting", "version", "dev", "addr", cfg.API.Addr)

	go mqttSrv.Run(ctx)

	if cfg.Integrations.ESPHome.Enabled {
		go esphome.NewManager(cfg.Integrations.ESPHome, store, eventBus).Run(ctx)
	}

	if cfg.Integrations.Sonoff.Enabled {
		go sonoff.NewManager(cfg.Integrations.Sonoff, store, eventBus).Run(ctx)
	}

	if cfg.Integrations.WiZ.Enabled {
		go wiz.NewManager(cfg.Integrations.WiZ, store, eventBus).Run(ctx)
	}

	if cfg.Integrations.WLED.Enabled {
		go wled.NewManager(cfg.Integrations.WLED, store, eventBus).Run(ctx)
	}

	if cfg.Integrations.Disagg.Enabled {
		go disagg.NewManager(cfg.Integrations.Disagg, store).Run(ctx)
	}
	if cfg.Integrations.Weather.Enabled {
		go weather.NewManager(cfg.Integrations.Weather, store).Run(ctx)
	}
	if cfg.Integrations.ClimateBrain.Enabled {
		go climatebrain.NewManager(cfg.Integrations.ClimateBrain, store, eventBus).Run(ctx)
	}
	if cfg.Integrations.Tigo.Enabled {
		go tigo.NewManager(cfg.Integrations.Tigo, store, eventBus).Run(ctx)
	}
	if cfg.Integrations.Rachio.Enabled {
		go rachio.NewManager(cfg.Integrations.Rachio, store, eventBus).Run(ctx)
	}

	// Virtual groups (room light groups etc.) — no-op if none configured.
	go groups.NewManager(cfg.Groups, store, eventBus).Run(ctx)

	if cfg.Integrations.LiquidFW.Enabled {
		lfw := liquidfw.NewManager(cfg.Integrations.LiquidFW, store, eventBus)
		go lfw.Run(ctx)
		apiSrv.SetDeviceRestart(lfw.Restart) // device-maintenance: reboot + OTA-prep from the UI
		apiSrv.SetDeviceOtaPrep(lfw.OtaPrep)
		apiSrv.SetDeviceEnterRecovery(lfw.EnterRecovery)

		// Thermostat brain: averages temp sensors and drives the on-device LiquidFW
		// thermostat (uses the liquidfw manager to push signed setpoint/mode/temp).
		if cfg.Integrations.Thermostat.Enabled {
			go thermostat.NewManager(cfg.Integrations.Thermostat, store, eventBus, lfw).Run(ctx)
		}

		// Publish LiquidFW entity state changes to MQTT for HA visibility.
		liquidfwDomains := map[string]bool{
			"sensor": true, "switch": true, "binary_sensor": true, "number": true,
		}
		eventBus.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
			p, ok := ev.Payload.(entity.StateChangedPayload)
			if !ok {
				return
			}
			e := p.Entity
			parts := strings.SplitN(e.ID, ".", 2)
			if len(parts) < 2 || !liquidfwDomains[parts[0]] {
				return
			}
			if _, isWiz := e.Attributes["wiz_mac"]; isWiz {
				return // WiZ is homeforge-only; never bridge to HA/MQTT
			}
			if _, isWled := e.Attributes["wled_host"]; isWled {
				return // WLED is homeforge-only
			}
			if attr, ok := e.Attributes["device"]; !ok || attr == nil {
				return // only publish entities that have "device" attr set by liquidfw
			}
			data, _ := json.Marshal(map[string]any{
				"state":      e.State,
				"attributes": e.Attributes,
			})
			mqttSrv.Publish(fmt.Sprintf("liquidfw/%s/state", e.ID), data)
		})
	}

	if err := apiSrv.Run(ctx); err != nil {
		slog.Error("api server error", "err", err)
	}

	slog.Info("HomeForge stopped")
}
