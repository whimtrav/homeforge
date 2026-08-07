package api

// Alexa Smart Home skill support (Phase 1: Discovery + PowerController +
// BrightnessController). HF exposes a curated set of entities (config
// integrations.alexa.devices) as Alexa endpoints and handles the Smart Home
// directives the Echo sends, translating them onto HF's own service bus.
//
// Transport: a thin AWS Lambda forwards Alexa directives to POST /api/alexa/directive
// with a Bearer shared token (same pattern as the media skill). This endpoint is
// exempt from the human-session gate (see authAllowed) and checks the shared token
// itself. Account-linking OAuth (required by Alexa to enable the skill) is a
// separate, later sub-step; it does not affect this directive handler.

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/whimtrav/homeforge/internal/config"
)

// ---- directive envelope (only the fields we use) ----

type alexaDirective struct {
	Directive struct {
		Header struct {
			Namespace        string `json:"namespace"`
			Name             string `json:"name"`
			PayloadVersion   string `json:"payloadVersion"`
			MessageID        string `json:"messageId"`
			CorrelationToken string `json:"correlationToken"`
		} `json:"header"`
		Endpoint struct {
			EndpointID string `json:"endpointId"`
			Scope      struct {
				Token string `json:"token"`
			} `json:"scope"`
		} `json:"endpoint"`
		Payload json.RawMessage `json:"payload"`
	} `json:"directive"`
}

// endpointId can't contain '.', so entity "switch.kitchen_sink" <-> "switch#kitchen_sink".
func entToEndpoint(e string) string { return strings.ReplaceAll(e, ".", "#") }
func endpointToEnt(e string) string { return strings.ReplaceAll(e, "#", ".") }

// brightness entity for a switch load, if one exists (WiZ/WLED expose 0-255).
func (s *Server) brightnessEntity(switchEnt string) (string, bool) {
	base := strings.TrimPrefix(switchEnt, "switch.")
	cand := "number." + base + "_brightness"
	if _, ok := s.store.Get(cand); ok {
		return cand, true
	}
	return "", false
}

// ---- HTTP entry point: POST /api/alexa/directive ----

func (s *Server) handleAlexaDirective(w http.ResponseWriter, r *http.Request) {
	// Shared-token auth for the Lambda->HF hop.
	if tok := strings.TrimSpace(s.alexaCfg.SharedToken); tok != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(tok)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var d alexaDirective
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	h := d.Directive.Header
	ep := endpointToEnt(d.Directive.Endpoint.EndpointID)

	var resp map[string]any
	switch h.Namespace {
	case "Alexa.Discovery":
		resp = s.alexaDiscovery(h.MessageID)

	case "Alexa.PowerController":
		on := h.Name == "TurnOn"
		svc := "switch.turn_off"
		if on {
			svc = "switch.turn_on"
		}
		s.bus.Publish("service.call", map[string]any{"service": svc, "entity": ep, "data": nil, "source": "alexa"})
		resp = s.alexaResponse(d, "Alexa.PowerController", "powerState", powerStr(on))

	case "Alexa.BrightnessController":
		var p struct {
			Brightness      int `json:"brightness"`
			BrightnessDelta int `json:"brightnessDelta"`
		}
		json.Unmarshal(d.Directive.Payload, &p)
		alexaBri := s.alexaSetBrightness(ep, h.Name, p.Brightness, p.BrightnessDelta)
		resp = s.alexaResponse(d, "Alexa.BrightnessController", "brightness", alexaBri)

	case "Alexa":
		if h.Name == "ReportState" {
			resp = s.alexaStateReport(d)
		}

	case "Alexa.Authorization": // AcceptGrant during account-linking — capture the grant for proactive events
		var g struct {
			Grant struct {
				Code string `json:"code"`
			} `json:"grant"`
		}
		json.Unmarshal(d.Directive.Payload, &g)
		s.alexaAcceptGrant(g.Grant.Code)
		resp = map[string]any{"event": map[string]any{
			"header":  alexaHeader("Alexa.Authorization", "AcceptGrant.Response", h.MessageID, ""),
			"payload": map[string]any{},
		}}
	}

	if resp == nil {
		resp = alexaError(d, "INVALID_DIRECTIVE", "unsupported directive")
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ---- discovery ----

func (s *Server) alexaDiscovery(msgID string) map[string]any {
	endpoints := []map[string]any{}
	for _, dev := range s.alexaCfg.Devices {
		ent, ok := s.store.Get(dev.Entity)
		if !ok {
			continue // not present right now; skip rather than advertise a dead endpoint
		}
		name := dev.Name
		if name == "" {
			name = ent.Name
		}
		if strings.EqualFold(dev.Category, "CONTACT_SENSOR") {
			endpoints = append(endpoints, map[string]any{
				"endpointId":        entToEndpoint(dev.Entity),
				"manufacturerName":  "HomeForge",
				"friendlyName":      name,
				"description":       name + " (HomeForge)",
				"displayCategories": []string{"CONTACT_SENSOR"},
				"capabilities": []map[string]any{
					alexaIface("Alexa", nil),
					alexaContactIface(),
					alexaIface("Alexa.EndpointHealth", []string{"connectivity"}),
				},
			})
			continue
		}
		cat := dev.Category
		if cat == "" {
			cat = "LIGHT"
			if strings.HasSuffix(dev.Entity, "_fan") {
				cat = "FAN"
			}
		}
		caps := []map[string]any{
			alexaIface("Alexa", nil),
			alexaIface("Alexa.PowerController", []string{"powerState"}),
			alexaIface("Alexa.EndpointHealth", []string{"connectivity"}),
		}
		if _, dim := s.brightnessEntity(dev.Entity); dim {
			caps = append(caps, alexaIface("Alexa.BrightnessController", []string{"brightness"}))
		}
		endpoints = append(endpoints, map[string]any{
			"endpointId":        entToEndpoint(dev.Entity),
			"manufacturerName":  "HomeForge",
			"friendlyName":      name,
			"description":       name + " (HomeForge)",
			"displayCategories": []string{cat},
			"capabilities":      caps,
		})
	}
	slog.Info("alexa: discovery", "endpoints", len(endpoints))
	return map[string]any{"event": map[string]any{
		"header":  alexaHeader("Alexa.Discovery", "Discover.Response", msgID, ""),
		"payload": map[string]any{"endpoints": endpoints},
	}}
}

// ---- brightness: Alexa 0-100 <-> HF 0-255 ----

func (s *Server) alexaSetBrightness(switchEnt, name string, bri, delta int) int {
	be, ok := s.brightnessEntity(switchEnt)
	if !ok {
		return 100
	}
	target := bri // SetBrightness gives an absolute 0-100
	if name == "AdjustBrightness" {
		cur := 0
		if e, ok := s.store.Get(be); ok {
			if v, err := strconv.Atoi(e.State); err == nil {
				cur = int(math.Round(float64(v) * 100.0 / 255.0))
			}
		}
		target = cur + delta
	}
	if target < 0 {
		target = 0
	}
	if target > 100 {
		target = 100
	}
	hf := int(math.Round(float64(target) * 255.0 / 100.0))
	// turning brightness up implies the load is on
	s.bus.Publish("service.call", map[string]any{"service": "switch.turn_on", "entity": switchEnt, "data": nil, "source": "alexa"})
	s.bus.Publish("service.call", map[string]any{"service": "number.set_value", "entity": be, "data": map[string]any{"value": hf}, "source": "alexa"})
	return target
}

// ---- state report ----

func (s *Server) alexaStateReport(d alexaDirective) map[string]any {
	ent := endpointToEnt(d.Directive.Endpoint.EndpointID)
	props := []map[string]any{alexaProp("Alexa.EndpointHealth", "connectivity", map[string]any{"value": "OK"})}
	if e, ok := s.store.Get(ent); ok {
		if strings.HasPrefix(ent, "binary_sensor.") {
			ds := "NOT_DETECTED"
			if strings.EqualFold(e.State, "on") {
				ds = "DETECTED"
			}
			props = append(props, alexaProp("Alexa.ContactSensor", "detectionState", ds))
		} else {
			props = append(props, alexaProp("Alexa.PowerController", "powerState", powerStr(strings.EqualFold(e.State, "on"))))
			if be, dim := s.brightnessEntity(ent); dim {
				if b, ok := s.store.Get(be); ok {
					if v, err := strconv.Atoi(b.State); err == nil {
						props = append(props, alexaProp("Alexa.BrightnessController", "brightness", int(math.Round(float64(v)*100.0/255.0))))
					}
				}
			}
		}
	}
	return map[string]any{
		"event": map[string]any{
			"header":   alexaHeader("Alexa", "StateReport", d.Directive.Header.MessageID, d.Directive.Header.CorrelationToken),
			"endpoint": map[string]any{"endpointId": d.Directive.Endpoint.EndpointID},
			"payload":  map[string]any{},
		},
		"context": map[string]any{"properties": props},
	}
}

// ---- response + helper builders ----

func (s *Server) alexaResponse(d alexaDirective, propNS, propName string, propVal any) map[string]any {
	return map[string]any{
		"event": map[string]any{
			"header":   alexaHeader("Alexa", "Response", d.Directive.Header.MessageID, d.Directive.Header.CorrelationToken),
			"endpoint": map[string]any{"endpointId": d.Directive.Endpoint.EndpointID},
			"payload":  map[string]any{},
		},
		"context": map[string]any{"properties": []map[string]any{
			alexaProp(propNS, propName, propVal),
			alexaProp("Alexa.EndpointHealth", "connectivity", map[string]any{"value": "OK"}),
		}},
	}
}

func alexaHeader(ns, name, msgID, corr string) map[string]any {
	h := map[string]any{"namespace": ns, "name": name, "payloadVersion": "3", "messageId": msgID}
	if corr != "" {
		h["correlationToken"] = corr
	}
	return h
}

func alexaProp(ns, name string, val any) map[string]any {
	return map[string]any{
		"namespace":                 ns,
		"name":                      name,
		"value":                     val,
		"timeOfSample":              time.Now().UTC().Format(time.RFC3339),
		"uncertaintyInMilliseconds": 500,
	}
}

func alexaIface(iface string, supported []string) map[string]any {
	m := map[string]any{"type": "AlexaInterface", "interface": iface, "version": "3"}
	if supported != nil {
		props := make([]map[string]any, 0, len(supported))
		for _, p := range supported {
			props = append(props, map[string]any{"name": p})
		}
		m["properties"] = map[string]any{"supported": props, "proactivelyReported": false, "retrievable": true}
	}
	return m
}

func alexaContactIface() map[string]any {
	return map[string]any{
		"type": "AlexaInterface", "interface": "Alexa.ContactSensor", "version": "3",
		"properties": map[string]any{
			"supported":           []map[string]any{{"name": "detectionState"}},
			"proactivelyReported": true,
			"retrievable":         true,
		},
	}
}

func alexaError(d alexaDirective, errType, msg string) map[string]any {
	return map[string]any{"event": map[string]any{
		"header":   alexaHeader("Alexa", "ErrorResponse", d.Directive.Header.MessageID, d.Directive.Header.CorrelationToken),
		"endpoint": map[string]any{"endpointId": d.Directive.Endpoint.EndpointID},
		"payload":  map[string]any{"type": errType, "message": msg},
	}}
}

func powerStr(on bool) string {
	if on {
		return "ON"
	}
	return "OFF"
}

// SetAlexa wires the curated Alexa device list + shared token from config.
func (s *Server) SetAlexa(c config.AlexaConfig) {
	s.alexaCfg = c
	if c.Enabled {
		s.seedBillReminder()
	}
}
