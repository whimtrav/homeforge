// HomeForge push relay — a tiny stateless service that forwards notifications to Firebase
// Cloud Messaging (FCM). Self-hosters' HomeForge backends POST here; the relay holds the FCM
// credentials (via the attached service account / ADC on Cloud Run — no key file) and fans
// the message out to the given device tokens. Nothing is stored; scale/move it freely.
//
// This is the HA-companion model: the app ships a public Firebase config, and this one relay
// holds the private sender identity, so individual self-hosters need zero Firebase setup.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
)

// pushReq is what a HomeForge backend POSTs to /push.
type pushReq struct {
	Tokens  []string          `json:"tokens"`            // FCM device tokens to deliver to
	Title   string            `json:"title"`             // notification title
	Message string            `json:"message"`           // notification body
	Image   string            `json:"image,omitempty"`   // optional image URL (e.g. camera snapshot)
	Channel string            `json:"channel,omitempty"` // Android channel id (loud/silent/critical)
	Tag     string            `json:"tag,omitempty"`     // collapse/replace key
	Data    map[string]string `json:"data,omitempty"`    // extra data (deep links, action ids)
}

type pushResp struct {
	Success       int      `json:"success"`
	Failure       int      `json:"failure"`
	InvalidTokens []string `json:"invalid_tokens,omitempty"` // caller should prune these
}

var fcm *messaging.Client

// crude in-memory per-IP rate limiter (60 req/min). Stateless across instances; good enough
// as an abuse backstop on a public endpoint. Real per-instance auth can come later.
var (
	rlMu     sync.Mutex
	rlBucket = map[string][]time.Time{}
)

const maxPerMin = 60

func rateOK(ip string) bool {
	rlMu.Lock()
	defer rlMu.Unlock()
	cutoff := time.Now().Add(-time.Minute)
	kept := rlBucket[ip][:0]
	for _, t := range rlBucket[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxPerMin {
		rlBucket[ip] = kept
		return false
	}
	rlBucket[ip] = append(kept, time.Now())
	return true
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !rateOK(clientIP(r)) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	var req pushReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.Tokens) == 0 {
		http.Error(w, "no tokens", http.StatusBadRequest)
		return
	}
	if len(req.Tokens) > 500 { // FCM multicast cap
		req.Tokens = req.Tokens[:500]
	}

	msg := &messaging.MulticastMessage{
		Tokens: req.Tokens,
		Notification: &messaging.Notification{
			Title:    req.Title,
			Body:     req.Message,
			ImageURL: req.Image,
		},
		Data: req.Data,
		Android: &messaging.AndroidConfig{
			Priority: "high",
			Notification: &messaging.AndroidNotification{
				Title:     req.Title,
				Body:      req.Message,
				ImageURL:  req.Image,
				ChannelID: req.Channel,
				Tag:       req.Tag,
			},
		},
	}

	br, err := fcm.SendEachForMulticast(r.Context(), msg)
	if err != nil {
		log.Printf("fcm send error: %v", err)
		http.Error(w, "fcm send failed", http.StatusBadGateway)
		return
	}

	resp := pushResp{Success: br.SuccessCount, Failure: br.FailureCount}
	for i, r2 := range br.Responses {
		if !r2.Success && messaging.IsRegistrationTokenNotRegistered(r2.Error) {
			resp.InvalidTokens = append(resp.InvalidTokens, req.Tokens[i])
		}
	}
	log.Printf("push: success=%d failure=%d invalid=%d", resp.Success, resp.Failure, len(resp.InvalidTokens))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func main() {
	ctx := context.Background()
	app, err := firebase.NewApp(ctx, nil) // ADC: uses the attached service account on Cloud Run
	if err != nil {
		log.Fatalf("firebase init: %v", err)
	}
	fcm, err = app.Messaging(ctx)
	if err != nil {
		log.Fatalf("messaging init: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/push", handlePush)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("homeforge push relay listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
