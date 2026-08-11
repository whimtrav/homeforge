// Package push manages FCM device tokens and delivers notifications through the HomeForge
// push relay. Android/iOS background push must go through Firebase Cloud Messaging, which is
// tied to a private sender identity you can't ship in an open-source app. So — like the Home
// Assistant companion app — a small stateless relay (see push-relay/, deployed on Cloud Run)
// holds the sender credentials, and every HomeForge backend just POSTs {tokens,title,...} to
// it. Self-hosters need zero Firebase setup; the app ships a public Firebase client config.
//
// Device tokens are stored per-install in a small JSON file (TokensFile). The app registers
// its token via POST /api/push/register; notify.* automation actions fan out to every token.
package push

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// DefaultRelayURL is the shared HomeForge push relay. Self-hosters get push for free with zero
// Firebase setup; override with mqtt.push_relay_url in config to point at your own relay.
const DefaultRelayURL = "https://homeforge-push-680299720805.us-central1.run.app/push"

// TokensFile holds the registered FCM device tokens (one JSON array of strings).
const TokensFile = "/data/push-tokens.json"

// mu serializes all reads/writes of TokensFile (small file, infrequent access).
var mu sync.Mutex

// Payload is one notification to fan out to every registered device.
type Payload struct {
	Title   string            // notification title
	Message string            // notification body
	Image   string            // optional image URL (camera snapshot)
	Channel string            // Android channel id: doorbell | motion | critical | default
	Tag     string            // collapse/replace key so repeats replace rather than stack
	Data    map[string]string // extra data (deep-link target, action ids)
}

type relayReq struct {
	Tokens  []string          `json:"tokens"`
	Title   string            `json:"title"`
	Message string            `json:"message"`
	Image   string            `json:"image,omitempty"`
	Channel string            `json:"channel,omitempty"`
	Tag     string            `json:"tag,omitempty"`
	Data    map[string]string `json:"data,omitempty"`
}

type relayResp struct {
	Success       int      `json:"success"`
	Failure       int      `json:"failure"`
	InvalidTokens []string `json:"invalid_tokens"`
}

// LoadTokens returns the registered device tokens (nil if the file is missing or corrupt).
func LoadTokens() []string {
	mu.Lock()
	defer mu.Unlock()
	return loadLocked()
}

func loadLocked() []string {
	b, err := os.ReadFile(TokensFile)
	if err != nil {
		return nil
	}
	var toks []string
	if json.Unmarshal(b, &toks) != nil {
		return nil
	}
	return toks
}

func saveLocked(toks []string) {
	b, _ := json.MarshalIndent(toks, "", "  ")
	tmp := TokensFile + ".tmp"
	if os.WriteFile(tmp, b, 0644) == nil {
		_ = os.Rename(tmp, TokensFile)
	}
}

// Add registers a device token (idempotent). Returns the new total token count.
func Add(token string) int {
	mu.Lock()
	defer mu.Unlock()
	toks := loadLocked()
	for _, t := range toks {
		if t == token {
			return len(toks)
		}
	}
	toks = append(toks, token)
	saveLocked(toks)
	return len(toks)
}

// Remove unregisters a device token (e.g. on logout).
func Remove(token string) {
	mu.Lock()
	defer mu.Unlock()
	toks := loadLocked()
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if t != token {
			out = append(out, t)
		}
	}
	saveLocked(out)
}

func prune(invalid []string) {
	if len(invalid) == 0 {
		return
	}
	bad := make(map[string]bool, len(invalid))
	for _, t := range invalid {
		bad[t] = true
	}
	mu.Lock()
	defer mu.Unlock()
	toks := loadLocked()
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if !bad[t] {
			out = append(out, t)
		}
	}
	if len(out) != len(toks) {
		saveLocked(out)
	}
}

// Send fans a payload out to every registered token via the relay, then prunes any tokens FCM
// reports as no-longer-registered. No-op when there are no tokens. relayURL defaults to
// DefaultRelayURL when empty.
func Send(relayURL string, p Payload) {
	if relayURL == "" {
		relayURL = DefaultRelayURL
	}
	toks := LoadTokens()
	if len(toks) == 0 {
		return
	}
	body, _ := json.Marshal(relayReq{
		Tokens: toks, Title: p.Title, Message: p.Message, Image: p.Image,
		Channel: p.Channel, Tag: p.Tag, Data: p.Data,
	})
	req, err := http.NewRequest(http.MethodPost, relayURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		slog.Warn("push: relay post failed", "err", err)
		return
	}
	defer resp.Body.Close()
	var rr relayResp
	_ = json.NewDecoder(resp.Body).Decode(&rr)
	slog.Info("push: sent", "title", p.Title, "success", rr.Success, "failure", rr.Failure, "invalid", len(rr.InvalidTokens))
	prune(rr.InvalidTokens)
}
