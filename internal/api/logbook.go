package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whimtrav/homeforge/internal/entity"
)

// logbook records discrete device state changes and WHY they happened (which automation, a
// manual/app action, the assistant, or the device itself), so the assistant can answer
// "why did the family room fan cut off" / "when did the porch light come on". Backed by the bus:
// service.call events carry a cause; the following state change is tagged with it.
// This is a CAPABILITY (one tool covers the whole class of "why/when did X change" questions),
// not a per-scenario trained behavior.

const logbookPath = "/data/logbook.jsonl"
const logbookMax = 1500

type logEntry struct {
	TS     int64  `json:"ts"` // unix seconds
	Entity string `json:"entity"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Cause  string `json:"cause"` // "automation:<name>" | "user" | "assistant" | "device" | ...
}

type cmdInfo struct {
	cause string
	ts    int64
}

type logbook struct {
	mu      sync.Mutex
	entries []logEntry
	lastCmd map[string]cmdInfo
	f       *os.File
}

// logbookDomains: only DISCRETE device changes are worth a logbook entry. Continuous numeric
// sensors (power, temperature, humidity) would flood it and aren't "events" a person asks about.
var logbookDomains = map[string]bool{
	"switch": true, "light": true, "fan": true, "lock": true, "climate": true,
	"cover": true, "binary_sensor": true, "media_player": true, "alarm_control_panel": true,
}

func newLogbook(path string) *logbook {
	l := &logbook{lastCmd: map[string]cmdInfo{}}
	// load the tail of the persisted log so history survives restarts
	if data, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(data)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			var e logEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				l.entries = append(l.entries, e)
			}
		}
		data.Close()
		if len(l.entries) > logbookMax {
			l.entries = l.entries[len(l.entries)-logbookMax:]
		}
	}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		l.f = f
	}
	return l
}

// noteCommand remembers who/what just commanded an entity, so the state change it produces can
// be attributed. Called on every service.call.
func (l *logbook) noteCommand(entityID, cause string) {
	if entityID == "" || cause == "" {
		return
	}
	l.mu.Lock()
	l.lastCmd[entityID] = cmdInfo{cause: cause, ts: time.Now().Unix()}
	l.mu.Unlock()
}

// noteState records a discrete state change with its attributed cause.
func (l *logbook) noteState(e entity.Entity) {
	dom := e.ID
	if i := strings.IndexByte(dom, '.'); i >= 0 {
		dom = dom[:i]
	}
	if !logbookDomains[dom] {
		return
	}
	// a state that is purely numeric is a reading, not a device event — skip
	if _, err := strconv.ParseFloat(strings.TrimSpace(e.State), 64); err == nil {
		return
	}
	now := time.Now().Unix()
	cause := "device" // autonomous device report or physical/manual toggle
	l.mu.Lock()
	if c, ok := l.lastCmd[e.ID]; ok && now-c.ts <= 15 {
		cause = c.cause
	}
	// de-dupe: skip if identical to the most recent entry for this entity
	for i := len(l.entries) - 1; i >= 0 && i >= len(l.entries)-8; i-- {
		if l.entries[i].Entity == e.ID {
			if l.entries[i].State == e.State {
				l.mu.Unlock()
				return
			}
			break
		}
	}
	ent := logEntry{TS: now, Entity: e.ID, Name: e.Name, State: e.State, Cause: cause}
	l.entries = append(l.entries, ent)
	if len(l.entries) > logbookMax {
		l.entries = l.entries[len(l.entries)-logbookMax:]
	}
	f := l.f
	l.mu.Unlock()
	if f != nil {
		if b, err := json.Marshal(ent); err == nil {
			f.Write(append(b, '\n'))
		}
	}
}

// recentEvents is the assistant tool: recent state changes for a device (or the whole house)
// with WHY each happened. One capability for the whole "why/when did X change" class.
func (s *Server) recentEvents(query string) string {
	if s.logbook == nil {
		return "no event log available yet"
	}
	ents := s.logbook.recent(query, 8)
	if len(ents) == 0 {
		if strings.TrimSpace(query) != "" {
			return "no recent changes recorded for " + query
		}
		return "no recent device changes recorded"
	}
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.Local
	}
	var parts []string
	for _, e := range ents {
		cause := e.Cause
		if strings.HasPrefix(cause, "automation:") {
			cause = "automation '" + strings.TrimPrefix(cause, "automation:") + "'"
		} else if cause == "device" {
			cause = "the device itself (manual/physical or a device report)"
		} else if cause == "user" {
			cause = "a manual action in the app"
		}
		when := time.Unix(e.TS, 0).In(loc).Format("Mon 3:04 PM")
		parts = append(parts, fmt.Sprintf("%s — %s → %s (cause: %s)", when, e.Name, e.State, cause))
	}
	return strings.Join(parts, "\n")
}

// recent returns up to limit newest-first entries, optionally filtered to entities whose
// id/name contains the (lowercased) filter tokens.
func (l *logbook) recent(filter string, limit int) []logEntry {
	toks := strings.FieldsFunc(strings.ToLower(filter), func(r rune) bool {
		return r == '_' || r == '.' || r == ' ' || r == '-'
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []logEntry
	for i := len(l.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := l.entries[i]
		if len(toks) > 0 {
			hay := strings.ToLower(e.Entity + " " + e.Name)
			match := true
			for _, t := range toks {
				if len(t) >= 2 && !strings.Contains(hay, t) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}
