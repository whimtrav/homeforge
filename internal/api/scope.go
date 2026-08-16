package api

// Per-account scoping. HomeForge already has multi-user auth (auth.go); this layers an OPTIONAL
// per-account restriction on top. An account WITHOUT a profile — the owner — is unrestricted and
// sees the whole house. A profile with is_owner:true is likewise unrestricted. Any other profile is
// SCOPED: the account may only read/control entities whose object-id (the part after the domain
// dot, e.g. "bedroom2_ceiling_light" in "switch.bedroom2_ceiling_light") starts with one of the
// profile's `allow` prefixes, plus its own health sensors (sensor.health_<health_person>_*). This is
// how Victor's account is locked to Bedroom 2. Persisted at /data/auth-profiles.json.

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

type userProfile struct {
	Email        string   `json:"email"`
	DisplayName  string   `json:"display_name"`
	IsOwner      bool     `json:"is_owner"`
	Allow        []string `json:"allow"`         // object-id prefixes, e.g. ["bedroom2","victors"]
	HealthPerson string   `json:"health_person"` // e.g. "victor"; own sensor.health_<slug>_* always allowed
	HomeRoom     string   `json:"home_room"`     // display label for the app's single-room home tab
	Tabs         []string `json:"tabs"`          // app tabs to show, e.g. ["home","health","ai"]
}

// defaultTabs is what an unrestricted (owner) account sees — the app's full tab set.
var defaultTabs = []string{"home", "cameras", "climate", "energy", "security", "health", "ai"}

// healthSlug mirrors the per-person slug the health ingest uses when building entity ids
// (sensor.health_<slug>_<metric>, see handleHealth), so a profile's health_person maps to the
// exact sensors that person's phone writes.
func healthSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// loadProfiles reads /data/auth-profiles.json into the store. Called from load() during init, so no
// locking is needed (mirrors how users/sessions load).
func (a *authStore) loadProfiles() {
	if a.profilesPath == "" {
		return
	}
	data, err := os.ReadFile(a.profilesPath)
	if err != nil {
		return
	}
	var ps []userProfile
	if json.Unmarshal(data, &ps) != nil {
		return
	}
	for _, p := range ps {
		a.profiles[strings.ToLower(strings.TrimSpace(p.Email))] = p
	}
}

func (a *authStore) profileFor(email string) (userProfile, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.profiles[email]
	return p, ok
}

// userScope restricts what a single request may read/control. A nil *userScope means unrestricted;
// allows() is safe to call on a nil receiver (returns true), so callers don't need nil checks.
type userScope struct {
	allow        []string // object-id prefixes
	healthPrefix string   // e.g. "health_victor" ("" = none)
}

func (sc *userScope) allows(entityID string) bool {
	if sc == nil {
		return true
	}
	obj := entityID
	if i := strings.IndexByte(entityID, '.'); i >= 0 {
		obj = entityID[i+1:]
	}
	for _, p := range sc.allow {
		if p != "" && strings.HasPrefix(obj, p) {
			return true
		}
	}
	if sc.healthPrefix != "" && strings.HasPrefix(obj, sc.healthPrefix) {
		return true
	}
	return false
}

// scopeFor resolves the restriction for a request. Requests without a user session (device ingest,
// X-HF-Internal calls) and unrestricted accounts (owner / no profile / is_owner) return nil.
func (s *Server) scopeFor(r *http.Request) *userScope {
	if s.auth == nil {
		return nil
	}
	email, ok := s.sessionEmail(r)
	if !ok {
		return nil
	}
	prof, ok := s.auth.profileFor(email)
	if !ok || prof.IsOwner {
		return nil
	}
	hp := ""
	if p := healthSlug(prof.HealthPerson); p != "" {
		hp = "health_" + p
	}
	return &userScope{allow: prof.Allow, healthPrefix: hp}
}
