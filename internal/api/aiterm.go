package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// aiterm is the "complex reasoning" terminal: a real PTY (running an AI CLI like claude/codex,
// or a shell) bridged to an xterm.js front-end over a websocket. The HUMAN types into a genuinely
// interactive session — HF is just the terminal window — which keeps subscription use on the
// interactive (durable) side, and the AI CLI reaches HF's tools via the MCP server (/api/mcp).
//
// The AI CLI itself runs in the glibc "aiterm" sidecar (node + CLIs). When AITERM_PROXY is set,
// HF upgrades the browser websocket and PROXIES it to the sidecar (owner gate enforced HERE first,
// since HF is the only front door on the public tunnel). With no proxy configured it falls back to
// a local shell in this container (dev only). AITERM_CMD overrides the local-shell command.

func aitermCommand() (string, []string) {
	if c := os.Getenv("AITERM_CMD"); c != "" {
		return "/bin/sh", []string{"-lc", c}
	}
	return "/bin/sh", nil
}

// aitermAuthed gates the terminal (and the MCP endpoint) to the owner: a valid internal token
// (header or ?token=) OR a logged-in session. A web shell MUST NOT be open — HF is reachable over
// the public tunnel.
func (s *Server) aitermAuthed(r *http.Request) bool {
	tok := r.Header.Get("X-HF-Internal")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if s.internalToken != "" && tok == s.internalToken {
		return true
	}
	if email, _ := s.sessionEmail(r); email != "" { // any logged-in account; owner-scope refinement later
		return true
	}
	return false
}

// handleAITermWS bridges the browser websocket to the AI terminal. With AITERM_PROXY set it relays
// to the sidecar; otherwise it runs a local PTY. Binary ws messages = keystrokes; text ws messages
// = a {"t":"r","cols":C,"rows":R} resize.
func (s *Server) handleAITermWS(w http.ResponseWriter, r *http.Request) {
	if !s.aitermAuthed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	if target := os.Getenv("AITERM_PROXY"); target != "" {
		s.aitermProxy(conn, target)
		return
	}
	s.aitermLocalPTY(conn)
}

// aitermProxy relays the (already-gated) browser websocket to the sidecar's PTY websocket,
// forwarding a shared secret so the sidecar only accepts HF-originated connections.
func (s *Server) aitermProxy(client *websocket.Conn, target string) {
	if sec := os.Getenv("AITERM_SECRET"); sec != "" {
		if u, err := url.Parse(target); err == nil {
			q := u.Query()
			q.Set("key", sec)
			u.RawQuery = q.Encode()
			target = u.String()
		}
	}
	up, _, err := websocket.DefaultDialer.Dial(target, nil)
	if err != nil {
		slog.Warn("aiterm: sidecar dial failed", "err", err)
		_ = client.WriteMessage(websocket.TextMessage, []byte("\r\nAI terminal backend unavailable. Is the 'aiterm' sidecar running (docker compose --profile ai up -d)?\r\n"))
		return
	}
	defer up.Close()
	slog.Info("aiterm: proxying to sidecar")

	done := make(chan struct{}, 2)
	go func() { // sidecar -> browser
		for {
			mt, data, err := up.ReadMessage()
			if err != nil {
				done <- struct{}{}
				return
			}
			if client.WriteMessage(mt, data) != nil {
				done <- struct{}{}
				return
			}
		}
	}()
	go func() { // browser -> sidecar
		for {
			mt, data, err := client.ReadMessage()
			if err != nil {
				done <- struct{}{}
				return
			}
			if up.WriteMessage(mt, data) != nil {
				done <- struct{}{}
				return
			}
		}
	}()
	<-done
}

// aitermLocalPTY runs a PTY in THIS container (dev fallback when no sidecar is configured).
func (s *Server) aitermLocalPTY(conn *websocket.Conn) {
	name, args := aitermCommand()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("failed to start terminal: "+err.Error()))
		return
	}
	defer func() { _ = ptmx.Close(); _ = cmd.Process.Kill() }()

	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				s.writeMu.Lock()
				werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n])
				s.writeMu.Unlock()
				if werr != nil {
					return
				}
			}
			if err != nil {
				conn.Close()
				return
			}
		}
	}()

	for {
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

// handleAITermPage serves the xterm.js front-end.
func (s *Server) handleAITermPage(w http.ResponseWriter, r *http.Request) {
	if !s.aitermAuthed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	slog.Info("aiterm: page served")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(aitermHTML))
}

const aitermHTML = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no">
<title>HomeForge · AI Terminal</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.min.css">
<style>
html,body{margin:0;height:100%;background:#0b1220;overflow:hidden}
#t{height:100%;padding:6px;box-sizing:border-box}
.xterm-viewport{-webkit-overflow-scrolling:touch}
#btm{position:fixed;right:14px;bottom:16px;width:46px;height:46px;border-radius:50%;
  border:1px solid #2a3550;background:#1b2740;color:#cfe3ff;font-size:22px;line-height:44px;
  text-align:center;display:none;z-index:9;box-shadow:0 4px 14px rgba(0,0,0,.5);opacity:.94}
</style>
</head><body>
<div id="t"></div>
<button id="btm" aria-label="Jump to newest">&#8595;</button>
<script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.10.0/lib/addon-fit.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-canvas@0.7.0/lib/addon-canvas.min.js"></script>
<script>
const q=new URLSearchParams(location.search), tok=q.get('token')||'', initPrompt=q.get('q')||'';
const wrap=document.getElementById('t');
const term=new Terminal({fontSize:13,scrollback:8000,cursorBlink:true,scrollSensitivity:3,
  smoothScrollDuration:0,theme:{background:'#0b1220'}});
const fit=new FitAddon.FitAddon(); term.loadAddon(fit);
term.open(wrap);
try{ term.loadAddon(new CanvasAddon.CanvasAddon()); }catch(e){} // fast GPU-ish rendering (smooth scroll)
fit.fit();
const proto=location.protocol==='https:'?'wss':'ws';
const ws=new WebSocket(proto+'://'+location.host+'/api/aiterm/ws'+(tok?('?token='+encodeURIComponent(tok)):''));
ws.binaryType='arraybuffer';
const enc=new TextEncoder(), dec=new TextDecoder();
ws.onopen=()=>{sendSize();term.focus();
  // Escalation from "Ask Claude": type the carried prompt into the session once the CLI is ready.
  if(initPrompt){setTimeout(()=>{if(ws.readyState===1){ws.send(enc.encode(initPrompt));setTimeout(()=>{if(ws.readyState===1)ws.send(enc.encode('\r'));},450);}},2800);}
};
ws.onmessage=e=>{term.write(typeof e.data==='string'?e.data:dec.decode(e.data));};
ws.onclose=()=>term.write('\r\n\x1b[31m[session closed]\x1b[0m\r\n');
term.onData(d=>{if(ws.readyState===1)ws.send(enc.encode(d));});
function sendSize(){if(ws.readyState===1)ws.send(JSON.stringify({t:'r',cols:term.cols,rows:term.rows}));}
addEventListener('resize',()=>{fit.fit();sendSize();});

// --- mobile: reliable swipe-to-scroll (drives the buffer directly) + jump-to-newest ---
function rowH(){try{return term._core._renderService.dimensions.css.cell.height||18;}catch(e){return 18;}}
let sy=0, active=false;
wrap.addEventListener('touchstart',function(e){if(e.touches.length===1){sy=e.touches[0].clientY;active=true;}},{passive:true});
wrap.addEventListener('touchmove',function(e){
  if(!active||e.touches.length!==1)return;
  var y=e.touches[0].clientY, dy=y-sy, h=rowH();
  if(Math.abs(dy)>=h){ term.scrollLines(-Math.trunc(dy/h)); sy=y; e.preventDefault(); }
},{passive:false});
wrap.addEventListener('touchend',function(){active=false;},{passive:true});
const btm=document.getElementById('btm');
term.onScroll(function(){var b=term.buffer.active; btm.style.display=(b.viewportY<b.baseY-1)?'block':'none';});
btm.addEventListener('click',function(){term.scrollToBottom(); btm.style.display='none';});
</script></body></html>`
