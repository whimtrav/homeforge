package api

// Alexa proactive events — the piece that lets HomeForge make Alexa SPEAK.
//
// A Smart Home skill can't push speech directly. The pattern (same as Home Assistant's Alexa
// integration): expose a virtual CONTACT SENSOR ("Bill Reminder"); when HF wants Alexa to
// announce, POST a proactive ChangeReport to the Alexa Event Gateway flipping the sensor to
// DETECTED. The user makes ONE Alexa Routine ("When Bill Reminder detects -> Announce '...'")
// which then fires. To send events HF needs an access token, obtained from the
// Alexa.Authorization/AcceptGrant directive delivered during account-linking (requires the
// skill's "Send Alexa Events" permission). We exchange that grant for a refresh token and mint
// short-lived access tokens against LWA on demand.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/whimtrav/homeforge/internal/entity"
)

const (
	billReminderEntity = "binary_sensor.bill_reminder"
	alexaEventStateF   = "/data/alexa-event.json"
	alexaEventGateway  = "https://api.amazonalexa.com/v3/events" // NA region gateway
	lwaTokenURL        = "https://api.amazon.com/auth/o2/token"
)

type alexaEventState struct {
	RefreshToken      string `json:"refresh_token"`
	LastReminderMonth string `json:"last_reminder_month"` // "2026-08" — reminder fires once/month
}

var (
	alexaEventMu   sync.Mutex // guards the state file
	alexaAccessMu  sync.Mutex // guards the cached access token
	alexaAccessTok string
	alexaAccessExp time.Time
)

func loadAlexaEventState() alexaEventState {
	var st alexaEventState
	if b, err := os.ReadFile(alexaEventStateF); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func saveAlexaEventState(st alexaEventState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := alexaEventStateF + ".tmp"
	if os.WriteFile(tmp, b, 0600) == nil {
		_ = os.Rename(tmp, alexaEventStateF)
	}
}

type lwaToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func lwaTokenRequest(form url.Values) (lwaToken, error) {
	var out lwaToken
	req, _ := http.NewRequest("POST", lwaTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("lwa %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

// alexaAcceptGrant exchanges the account-linking grant code for a refresh token so HF can call
// the event gateway. Invoked from the Alexa.Authorization/AcceptGrant directive handler.
func (s *Server) alexaAcceptGrant(code string) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(s.alexaCfg.EventClientID) == "" {
		slog.Warn("alexa: AcceptGrant received but event_client_id is unset — proactive events stay off")
		return
	}
	tok, err := lwaTokenRequest(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {s.alexaCfg.EventClientID},
		"client_secret": {s.alexaCfg.EventClientSecret},
	})
	if err != nil {
		slog.Error("alexa: AcceptGrant token exchange failed", "err", err)
		return
	}
	alexaEventMu.Lock()
	st := loadAlexaEventState()
	st.RefreshToken = tok.RefreshToken
	saveAlexaEventState(st)
	alexaEventMu.Unlock()
	slog.Info("alexa: AcceptGrant stored — proactive events enabled")
}

// alexaEventAccessToken returns a valid Alexa event-gateway access token, refreshing via the
// stored grant when the cached one is stale.
func (s *Server) alexaEventAccessToken() (string, error) {
	alexaAccessMu.Lock()
	defer alexaAccessMu.Unlock()
	if alexaAccessTok != "" && time.Now().Before(alexaAccessExp) {
		return alexaAccessTok, nil
	}
	alexaEventMu.Lock()
	rt := loadAlexaEventState().RefreshToken
	alexaEventMu.Unlock()
	if rt == "" {
		return "", fmt.Errorf("no grant — link the skill with the 'Send Alexa Events' permission enabled")
	}
	tok, err := lwaTokenRequest(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		"client_id":     {s.alexaCfg.EventClientID},
		"client_secret": {s.alexaCfg.EventClientSecret},
	})
	if err != nil {
		return "", err
	}
	alexaAccessTok = tok.AccessToken
	alexaAccessExp = time.Now().Add(time.Duration(tok.ExpiresIn-60) * time.Second)
	if tok.RefreshToken != "" && tok.RefreshToken != rt { // LWA occasionally rotates it
		alexaEventMu.Lock()
		st := loadAlexaEventState()
		st.RefreshToken = tok.RefreshToken
		saveAlexaEventState(st)
		alexaEventMu.Unlock()
	}
	return alexaAccessTok, nil
}

func alexaMsgID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sendContactChangeReport pushes the Bill Reminder sensor's new detectionState to Alexa so a
// user Routine bound to it can fire.
func (s *Server) sendContactChangeReport(detected bool) error {
	tok, err := s.alexaEventAccessToken()
	if err != nil {
		return err
	}
	ds := "NOT_DETECTED"
	if detected {
		ds = "DETECTED"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	body := map[string]any{
		"event": map[string]any{
			"header": map[string]any{
				"namespace": "Alexa", "name": "ChangeReport", "payloadVersion": "3", "messageId": alexaMsgID(),
			},
			"endpoint": map[string]any{
				"scope":      map[string]any{"type": "BearerToken", "token": tok},
				"endpointId": entToEndpoint(billReminderEntity),
			},
			"payload": map[string]any{
				"change": map[string]any{
					"cause": map[string]any{"type": "APP_INTERACTION"},
					"properties": []map[string]any{{
						"namespace": "Alexa.ContactSensor", "name": "detectionState",
						"value": ds, "timeOfSample": now, "uncertaintyInMilliseconds": 0,
					}},
				},
			},
		},
		"context": map[string]any{"properties": []map[string]any{{
			"namespace": "Alexa.EndpointHealth", "name": "connectivity",
			"value": map[string]any{"value": "OK"}, "timeOfSample": now, "uncertaintyInMilliseconds": 0,
		}}},
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", alexaEventGateway, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("event gateway %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// seedBillReminder registers the virtual contact-sensor entity so discovery + ReportState work.
func (s *Server) seedBillReminder() {
	if _, ok := s.store.Get(billReminderEntity); !ok {
		s.store.Set(entity.Entity{
			ID: billReminderEntity, Name: "Bill Reminder", Domain: "binary_sensor",
			State: "off", Attributes: map[string]any{"device_class": "occupancy"},
		})
	}
}

// setBillReminder flips the virtual sensor and pushes the change to Alexa.
func (s *Server) setBillReminder(detected bool) {
	state := "off"
	if detected {
		state = "on"
	}
	s.store.Set(entity.Entity{
		ID: billReminderEntity, Name: "Bill Reminder", Domain: "binary_sensor",
		State: state, Attributes: map[string]any{"device_class": "occupancy"},
	})
	if err := s.sendContactChangeReport(detected); err != nil {
		slog.Warn("alexa: bill-reminder change report failed", "detected", detected, "err", err)
	}
}

// runAlexaReminders fires the monthly electric-bill reminder: on the 1st of the month (>=08:00
// local) it flips the Bill Reminder contact sensor to DETECTED so the user's Alexa Routine
// announces it. Once per month, skipped if a bill was already logged this month, and auto-reset
// ~6 min later so next month is a fresh DETECTED transition.
func (s *Server) runAlexaReminders(ctx context.Context) {
	if !s.alexaCfg.Enabled {
		return
	}
	loc := energyLoc()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	var resetAt time.Time

	check := func() {
		now := time.Now().In(loc)
		if !resetAt.IsZero() && now.After(resetAt) {
			s.setBillReminder(false)
			resetAt = time.Time{}
		}
		if now.Day() != 1 || now.Hour() < 8 {
			return
		}
		month := now.Format("2006-01")
		alexaEventMu.Lock()
		st := loadAlexaEventState()
		fired := st.LastReminderMonth == month
		alexaEventMu.Unlock()
		if fired {
			return
		}
		// The user may have logged the bill before the 1st — don't nag if so.
		if ec := loadEnergyCycle(); len(ec.Bills) > 0 &&
			strings.HasPrefix(ec.Bills[len(ec.Bills)-1].ClosedOn, month) {
			alexaEventMu.Lock()
			st.LastReminderMonth = month
			saveAlexaEventState(st)
			alexaEventMu.Unlock()
			return
		}
		slog.Info("alexa: firing monthly bill reminder", "month", month)
		s.setBillReminder(true)
		resetAt = now.Add(6 * time.Minute)
		alexaEventMu.Lock()
		st.LastReminderMonth = month
		saveAlexaEventState(st)
		alexaEventMu.Unlock()
	}

	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
