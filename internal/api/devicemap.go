package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
)

// deviceMap is the per-install "learn my home" semantic map produced by the mapper
// (homeforge-model/mapper). It grounds the GENERAL assistant model in THIS home's actual
// devices: type, room, human role, aliases, and reachability. Optional — if /data/device-map.json
// is absent or unreadable, the assistant falls back to the raw entity dump (no behavior change).
const deviceMapPath = "/data/device-map.json"

type deviceMapEntry struct {
	EntityID   string   `json:"entity_id"`
	Name       string   `json:"name"`
	Domain     string   `json:"domain"`
	Type       string   `json:"type"`
	Room       string   `json:"room"`
	Role       string   `json:"role"`
	Aliases    []string `json:"aliases"`
	Capability string   `json:"capability"`
	Source     string   `json:"source"`
	Salience   string   `json:"salience"`
	Online     bool     `json:"online"`
}

type deviceMapData struct {
	entries  []deviceMapEntry
	byID     map[string]deviceMapEntry
	aliasIdx map[string]string // lowercased alias/role/name -> entity_id (exact-match resolution)
}

func localSource(src string) bool {
	if strings.HasSuffix(src, "-local") {
		return true
	}
	switch src {
	case "liquidfw-local", "sentinel-nvr", "droplet-water", "hf-climate-brain":
		return true
	}
	return false
}

// loadDeviceMap reads and indexes the map. Returns nil (not an error) if the file is missing or
// malformed, so the assistant simply falls back to its raw device dump.
func loadDeviceMap(path string) *deviceMapData {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ents []deviceMapEntry
	if err := json.Unmarshal(raw, &ents); err != nil {
		slog.Warn("device map: parse failed, falling back to raw dump", "err", err)
		return nil
	}
	d := &deviceMapData{
		entries:  ents,
		byID:     make(map[string]deviceMapEntry, len(ents)),
		aliasIdx: make(map[string]string),
	}
	for _, e := range ents {
		d.byID[e.EntityID] = e
		add := func(k string) {
			k = strings.ToLower(strings.TrimSpace(k))
			if k != "" {
				if _, exists := d.aliasIdx[k]; !exists {
					d.aliasIdx[k] = e.EntityID
				}
			}
		}
		add(e.Role)
		add(e.Name)
		for _, a := range e.Aliases {
			add(a)
		}
	}
	slog.Info("device map loaded", "entities", len(ents), "aliases", len(d.aliasIdx))
	return d
}

// groundingSection returns the map-derived device grounding for the system prompt, or "" if no
// map is loaded — in which case the caller falls back to the raw entity dump.
func (s *Server) groundingSection() string {
	if s.devMap == nil {
		return ""
	}
	return s.devMap.grounding()
}

// grounding builds the assistant's device section from the map: only PRIMARY controllable devices,
// grouped by room, labeled by human role, WITH the exact entity_id and a cloud-reachability tag.
// Leaner than dumping every entity id+state (primary-only + grouped) and far more useful (role +
// room + reachability). Deterministic (sorted) so ollama still prompt-caches the static prefix.
func (d *deviceMapData) grounding() string {
	type dev struct {
		typ, role, id string
		cloud         bool
	}
	rooms := map[string][]dev{}
	for _, e := range d.entries {
		if e.Salience != "primary" || e.Capability != "controllable" {
			continue
		}
		room := e.Room
		if room == "" || room == "unknown" {
			room = "other"
		}
		role := e.Role
		if role == "" {
			role = e.Name
		}
		rooms[room] = append(rooms[room], dev{e.Type, role, e.EntityID, !localSource(e.Source)})
	}
	if len(rooms) == 0 {
		return ""
	}
	roomNames := make([]string, 0, len(rooms))
	for r := range rooms {
		roomNames = append(roomNames, r)
	}
	sort.Strings(roomNames)

	var b strings.Builder
	b.WriteString("Devices you can control (grouped by room). Call tools with the [entity_id]. A [cloud] tag means it depends on a third-party cloud and may lag or be offline.\n")
	for _, r := range roomNames {
		devs := rooms[r]
		sort.Slice(devs, func(i, j int) bool { return devs[i].id < devs[j].id })
		fmt.Fprintf(&b, "## %s\n", r)
		for _, dv := range devs {
			tag := ""
			if dv.cloud {
				tag = " [cloud]"
			}
			fmt.Fprintf(&b, "- %s: %s [%s]%s\n", dv.typ, dv.role, dv.id, tag)
		}
	}
	return b.String()
}

// resolveByMap resolves a natural-language phrase ("the blower", "outside temp", "kitchen lights")
// to an entity_id using the map's role/alias/name index. Exact index hit wins; otherwise a
// token-overlap score against each entry's role+aliases+name+id (same threshold spirit as
// resolveEntity). Returns "" if nothing is a confident match.
func (d *deviceMapData) resolveByMap(phrase string) string {
	p := strings.ToLower(strings.TrimSpace(phrase))
	if p == "" {
		return ""
	}
	if id, ok := d.aliasIdx[p]; ok {
		return id
	}
	// strip a (possibly wrong) domain the model may have prepended
	if i := strings.Index(p, "."); i >= 0 {
		if id, ok := d.aliasIdx[p[i+1:]]; ok {
			return id
		}
	}
	toks := strings.FieldsFunc(p, func(r rune) bool { return r == '_' || r == '.' || r == ' ' || r == '-' })
	bestID, bestScore := "", 0
	for _, e := range d.entries {
		if e.Capability != "controllable" {
			continue
		}
		hay := strings.ToLower(e.EntityID + " " + e.Name + " " + e.Role + " " + strings.Join(e.Aliases, " "))
		score := 0
		for _, t := range toks {
			if len(t) >= 3 && strings.Contains(hay, t) {
				score++
			}
		}
		if score > bestScore {
			bestScore, bestID = score, e.EntityID
		}
	}
	if bestScore >= 2 {
		return bestID
	}
	return ""
}
