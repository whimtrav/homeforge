// aitermd is the AI Terminal backend: a PTY<->websocket bridge that runs INSIDE the glibc
// "aiterm" sidecar (node + the AI CLIs). HomeForge's gated /api/aiterm/ws proxies to /ws here,
// so HF core stays lean (alpine, no node) while the AI CLI runs in a full Debian environment.
//
// It also exposes /gen: a ONE-SHOT generation endpoint (claude -p on the user's SUBSCRIPTION,
// no API key) used by HomeForge's matrix scene generator. Both are bound to localhost and
// require a shared secret (?key=) — the only reachable front door is HF, which enforces the
// owner gate before proxying / calling here.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// Keepalive: the browser auto-answers ping frames while the page is open, so an
// active session (even idle, being read) stays alive; a backgrounded/closed webview
// stops answering, the read deadline lapses, and the claude session is reaped.
const (
	pongWait   = 70 * time.Second
	pingPeriod = 30 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func secretOK(r *http.Request) bool {
	sec := os.Getenv("AITERM_SECRET")
	return sec == "" || r.URL.Query().Get("key") == sec
}

// command returns what to run in the interactive PTY. AITERM_CMD (set by the entrypoint to the
// chosen AI CLI, e.g. `claude --mcp-config ...`) is run under a login shell so PATH/env resolve;
// with none set it drops to an interactive bash.
func command() (string, []string) {
	if c := os.Getenv("AITERM_CMD"); c != "" {
		return "/bin/bash", []string{"-lc", c}
	}
	return "/bin/bash", []string{"-l"}
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	if !secretOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var mu sync.Mutex // serialize all writes to conn (PTY output + keepalive pings)
	write := func(mt int, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteMessage(mt, data)
	}

	name, args := command()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		_ = write(websocket.TextMessage, []byte("failed to start terminal: "+err.Error()))
		return
	}
	defer func() { _ = ptmx.Close(); _ = cmd.Process.Kill() }()

	done := make(chan struct{})
	defer close(done)

	// Keepalive: reap abandoned/backgrounded sessions.
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	go func() {
		t := time.NewTicker(pingPeriod)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if write(websocket.PingMessage, nil) != nil {
					return
				}
			}
		}
	}()

	go func() { // PTY output -> websocket (binary)
		buf := make([]byte, 8192)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if write(websocket.BinaryMessage, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				conn.Close()
				return
			}
		}
	}()

	for { // websocket -> PTY (binary = keystrokes; text = {"t":"r","cols","rows"} resize)
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.TextMessage {
			var m struct {
				T    string `json:"t"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &m) == nil && m.T == "r" && m.Cols > 0 {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: m.Cols, Rows: m.Rows})
				continue
			}
		}
		if _, err := ptmx.Write(data); err != nil {
			return
		}
	}
}

// handleGen runs the AI CLI in print mode (one-shot, no tools) using the sidecar's SUBSCRIPTION
// login — no Anthropic API key. Body: {"system":"...","prompt":"..."} -> {"text":"<model output>"}.
// Used by HomeForge's matrix scene generator so "deluxe" scenes come off the subscription.
func handleGen(w http.ResponseWriter, r *http.Request) {
	if !secretOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in struct {
		System string `json:"system"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Prompt == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	full := in.Prompt
	if in.System != "" {
		full = in.System + "\n\n" + in.Prompt
	}
	base := os.Getenv("GEN_CMD")
	if base == "" {
		base = "claude"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, base, "-p", full)
	cmd.Env = os.Environ() // subscription login lives in $HOME/.claude
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("gen error: %v :: %s", err, stderr.String())
		http.Error(w, "generation failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"text": stdout.String()})
}

func main() {
	addr := os.Getenv("AITERM_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7799"
	}
	http.HandleFunc("/ws", handleWS)
	http.HandleFunc("/gen", handleGen)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	log.Printf("aitermd listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
