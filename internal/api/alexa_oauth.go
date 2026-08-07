package api

// Alexa account-linking OAuth 2.0 (authorization-code grant). Alexa REQUIRES account
// linking before a Smart Home skill can be enabled, so HF speaks just enough OAuth to
// complete the link. Minimal and self-contained — no server-side token table:
//
//   GET  /api/alexa/oauth/authorize — Alexa sends the owner's browser here. If there's no
//        valid HF session we render a small login form (POSTs back to this same URL). Once a
//        session exists we mint a short-lived auth code and 302 back to Alexa's redirect_uri.
//   POST /api/alexa/oauth/authorize — the login form target (email + password + carried params).
//   POST /api/alexa/oauth/token     — Alexa's backend exchanges the code (or a refresh token)
//        for an access token. Authenticated by client_id/client_secret (config).
//
// Tokens are stateless HMAC blobs (payload.sig, payload = base64url(JSON{typ,exp}), sig =
// HMAC-SHA256(payload, oauthSecret)). The directive handler still authenticates the Lambda->HF
// hop with the shared token; these OAuth tokens exist so Alexa's link flow completes and so we
// can gate WHO is allowed to link (a valid HF account).

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Amazon's fixed account-linking redirect hosts. Allow-listed so the auth code can't be
// steered at an attacker's server (defense in depth — authorize also requires an HF login).
var alexaRedirectHosts = map[string]bool{
	"pitangui.amazon.com": true, // North America
	"layla.amazon.com":    true, // Europe
	"alexa.amazon.co.jp":  true, // Far East
}

// oauthSecret derives the token-signing key from the shared token, so there's one secret to
// manage. If shared_token is blank the OAuth endpoints refuse to run (see the guards below).
func (s *Server) oauthSecret() []byte {
	sum := sha256.Sum256([]byte("hf-alexa-oauth\x00" + s.alexaCfg.SharedToken))
	return sum[:]
}

func (s *Server) oauthClientID() string {
	if id := strings.TrimSpace(s.alexaCfg.OAuthClientID); id != "" {
		return id
	}
	return "homeforge-alexa"
}

func (s *Server) oauthConfigured() bool {
	return s.alexaCfg.Enabled &&
		strings.TrimSpace(s.alexaCfg.SharedToken) != "" &&
		strings.TrimSpace(s.alexaCfg.OAuthClientSecret) != ""
}

func (s *Server) mintOAuthToken(typ string, ttl time.Duration) string {
	payload, _ := json.Marshal(map[string]any{"typ": typ, "exp": time.Now().Add(ttl).Unix()})
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.oauthSecret())
	mac.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyOAuthToken(tok, wantTyp string) bool {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		return false
	}
	mac := hmac.New(sha256.New, s.oauthSecret())
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(want)) != 1 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var c struct {
		Typ string `json:"typ"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Typ != wantTyp {
		return false
	}
	return time.Now().Unix() < c.Exp
}

// GET/POST /api/alexa/oauth/authorize
func (s *Server) handleAlexaAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.oauthConfigured() {
		http.Error(w, "alexa account-linking is not configured", http.StatusNotFound)
		return
	}
	r.ParseForm() // parses query on GET, query+body on POST

	// POST = the login form submitting credentials. Verify, then fall through with a session.
	if r.Method == http.MethodPost {
		ip := clientIP(r)
		if s.auth.blocked(ip) {
			http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
			return
		}
		email, pass := r.FormValue("email"), r.FormValue("password")
		if !s.auth.verify(email, pass) {
			s.auth.recordFail(ip)
			s.renderAlexaLogin(w, r, "Invalid email or password.")
			return
		}
		s.auth.recordOK(ip)
		s.setSession(w, r, strings.ToLower(strings.TrimSpace(email)))
	}

	// Validate the OAuth request (r.FormValue reads carried hidden fields on POST, query on GET).
	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	state := r.FormValue("state")
	if rt := r.FormValue("response_type"); rt != "" && rt != "code" {
		http.Error(w, "unsupported response_type", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(clientID), []byte(s.oauthClientID())) != 1 {
		http.Error(w, "invalid client_id", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(redirectURI)
	if err != nil || !alexaRedirectHosts[u.Host] {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	// Require a valid HF session; if absent, show the login form (which POSTs back here).
	if _, ok := s.sessionEmail(r); !ok {
		s.renderAlexaLogin(w, r, "")
		return
	}

	// Authenticated — mint a one-time auth code and redirect back to Alexa.
	code := s.mintOAuthToken("code", 10*time.Minute)
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// POST /api/alexa/oauth/token — server-to-server from Amazon.
func (s *Server) handleAlexaToken(w http.ResponseWriter, r *http.Request) {
	if !s.oauthConfigured() {
		oauthErr(w, http.StatusNotFound, "invalid_request")
		return
	}
	r.ParseForm()
	// Client auth: params in the body, or HTTP Basic — Alexa can be configured for either.
	clientID, clientSecret := r.FormValue("client_id"), r.FormValue("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		clientID, clientSecret = u, p
	}
	if subtle.ConstantTimeCompare([]byte(clientID), []byte(s.oauthClientID())) != 1 ||
		subtle.ConstantTimeCompare([]byte(clientSecret), []byte(strings.TrimSpace(s.alexaCfg.OAuthClientSecret))) != 1 {
		oauthErr(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	switch r.FormValue("grant_type") {
	case "authorization_code":
		if !s.verifyOAuthToken(r.FormValue("code"), "code") {
			oauthErr(w, http.StatusBadRequest, "invalid_grant")
			return
		}
	case "refresh_token":
		if !s.verifyOAuthToken(r.FormValue("refresh_token"), "refresh") {
			oauthErr(w, http.StatusBadRequest, "invalid_grant")
			return
		}
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	writeJSON(w, map[string]any{
		"access_token":  s.mintOAuthToken("access", 24*time.Hour),
		"token_type":    "bearer",
		"expires_in":    86400,
		"refresh_token": s.mintOAuthToken("refresh", 3650*24*time.Hour),
	})
}

func oauthErr(w http.ResponseWriter, code int, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": errType})
}

func (s *Server) renderAlexaLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	hidden := ""
	for _, k := range []string{"client_id", "redirect_uri", "state", "response_type", "scope"} {
		if v := r.FormValue(k); v != "" {
			hidden += fmt.Sprintf("<input type=\"hidden\" name=\"%s\" value=\"%s\">", k, html.EscapeString(v))
		}
	}
	errHTML := ""
	if errMsg != "" {
		errHTML = `<p class="err">` + html.EscapeString(errMsg) + `</p>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, alexaLoginHTML, errHTML, hidden)
}

const alexaLoginHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Link HomeForge to Alexa</title>
<style>
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;
  font:16px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
  background:radial-gradient(1200px 600px at 50% -10%,#1b2331,#0d1015);color:#e7e9ee}
.card{width:min(92vw,360px);background:#171a21;border:1px solid #262b36;
  border-radius:16px;padding:30px 26px;box-shadow:0 18px 50px rgba(0,0,0,.45)}
.logo{font-size:12px;letter-spacing:.18em;text-transform:uppercase;color:#5b9dff;font-weight:800;margin-bottom:14px}
h1{margin:0 0 4px;font-size:20px}
p.sub{margin:0 0 18px;color:#8b93a5;font-size:13.5px}
label{display:block;font-size:12.5px;color:#aab1c0;margin:14px 0 5px}
input[type=email],input[type=password]{width:100%;padding:11px 12px;border-radius:10px;
  border:1px solid #2d3340;background:#10131a;color:#e7e9ee;font-size:15px}
input:focus{outline:2px solid #5b9dff;border-color:transparent}
button{margin-top:22px;width:100%;padding:12px;border:0;border-radius:10px;
  background:#4f8cff;color:#fff;font-size:15px;font-weight:700;cursor:pointer}
button:hover{background:#3f7bec}
.err{background:#3a1d22;border:1px solid #6b2b33;color:#ffb4bd;padding:9px 12px;border-radius:9px;font-size:13px;margin:0 0 2px}
</style></head><body>
<form class="card" method="post" action="/api/alexa/oauth/authorize">
  <div class="logo">HomeForge</div>
  <h1>Link to Alexa</h1>
  <p class="sub">Sign in with your HomeForge account so Alexa can control your home.</p>
  %s
  <label for="email">Email</label>
  <input id="email" type="email" name="email" autocomplete="username" required autofocus>
  <label for="password">Password</label>
  <input id="password" type="password" name="password" autocomplete="current-password" required>
  %s
  <button type="submit">Sign in &amp; link</button>
</form></body></html>`
