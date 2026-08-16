package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// mcp.go exposes HomeForge's assistant tools over the Model Context Protocol (streamable-HTTP
// transport) so an AI CLI (Claude Code / Codex / Gemini) running in the AI Terminal can read and
// control the house through the SAME tool implementations the local assistant uses (execTool +
// assistantTools). One tool surface, one code path, one auth gate.
//
// Owner-gated by the internal token (aitermAuthed) — HF answers on a public tunnel, so an
// unauthenticated caller must never reach the house's tools. Point a CLI at it with an
// mcp-config entry of type "http", url http://localhost:8093/api/mcp, header X-HF-Internal.

const mcpProtocolVersion = "2025-06-18"
const mcpServerVersion = "1.0"

type mcpReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// handleMCP is the streamable-HTTP MCP endpoint. POST carries one JSON-RPC message (or a batch);
// GET (a server->client SSE stream) is unsupported here — this is a request/response tool server;
// DELETE (session teardown) is a no-op since the server is stateless.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.aitermAuthed(r) { // same owner gate as the AI Terminal
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		http.Error(w, "no server stream", http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		// handled below
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.mcpWriteErr(w, nil, -32700, "read error")
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// JSON-RPC batch (array) vs single message.
	if trimmed[0] == '[' {
		var batch []mcpReq
		if json.Unmarshal(trimmed, &batch) != nil {
			s.mcpWriteErr(w, nil, -32700, "parse error")
			return
		}
		var out []mcpResp
		for _, req := range batch {
			if resp, ok := s.mcpDispatch(req); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	var req mcpReq
	if json.Unmarshal(trimmed, &req) != nil {
		s.mcpWriteErr(w, nil, -32700, "parse error")
		return
	}
	resp, ok := s.mcpDispatch(req)
	if !ok { // notification: no response body
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// mcpDispatch handles one JSON-RPC message. Returns (response, true) for requests, and
// (zero, false) for notifications (no id) which get no response.
func (s *Server) mcpDispatch(req mcpReq) (mcpResp, bool) {
	isNotification := len(req.ID) == 0
	resp := mcpResp{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		ver := mcpProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			ver = p.ProtocolVersion // echo the client's negotiated version
		}
		resp.Result = map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "homeforge", "version": mcpServerVersion},
			"instructions":    "HomeForge smart-home tools for THIS house. Read sensors and history, check climate/energy/water/presence, and control switches, lights, fans, and the thermostat. Prefer read_sensor/query_history/climate_status over guessing; controls act on the real house.",
		}

	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		return mcpResp{}, false

	case "ping":
		resp.Result = map[string]any{}

	case "tools/list":
		tools := make([]map[string]any, 0, len(assistantTools()))
		for _, t := range assistantTools() {
			params := t.Function.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			// An empty/nil "required" would marshal to null, which strict MCP clients reject.
			if req, ok := params["required"].([]string); ok && len(req) == 0 {
				delete(params, "required")
			}
			tools = append(tools, map[string]any{
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"inputSchema": params,
			})
		}
		resp.Result = map[string]any{"tools": tools}

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.Name == "" {
			resp.Error = &mcpError{Code: -32602, Message: "invalid params"}
			break
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		out := s.execTool(p.Name, p.Arguments)
		slog.Info("mcp: tool called", "tool", p.Name)
		resp.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": out}},
		}

	default:
		if isNotification {
			return mcpResp{}, false
		}
		resp.Error = &mcpError{Code: -32601, Message: "method not found: " + req.Method}
	}

	if isNotification {
		return mcpResp{}, false
	}
	return resp, true
}

func (s *Server) mcpWriteErr(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcpResp{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: msg}})
}
