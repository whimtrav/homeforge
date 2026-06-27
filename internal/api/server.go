package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	cfg   config.APIConfig
	store *entity.Store
	bus   *bus.Bus

	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}
}

func NewServer(cfg config.APIConfig, store *entity.Store, b *bus.Bus) *Server {
	s := &Server{
		cfg:     cfg,
		store:   store,
		bus:     b,
		clients: make(map[*websocket.Conn]struct{}),
	}

	// Broadcast every state change to all WebSocket clients.
	b.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
		payload, ok := ev.Payload.(entity.StateChangedPayload)
		if !ok {
			return
		}
		msg, _ := json.Marshal(map[string]any{
			"type":   "state_changed",
			"entity": payload.Entity,
		})
		s.broadcast(msg)
	})

	return s
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/entities", s.handleEntities)
	mux.HandleFunc("GET /api/entities/{id}", s.handleEntity)
	mux.HandleFunc("POST /api/services/{domain}/{service}", s.handleServiceCall)
	mux.HandleFunc("GET /api/ws", s.handleWebSocket)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Serve embedded frontend.
	mux.Handle("/", http.FileServer(http.FS(webFS())))

	addr := s.cfg.Addr
	if addr == "" {
		addr = ":8123"
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	slog.Info("api: listening", "addr", addr)

	go func() {
		<-ctx.Done()
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx2)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleEntities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.store.All())
}

func (s *Server) handleEntity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e, ok := s.store.Get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(e)
}

func (s *Server) handleServiceCall(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	service := r.PathValue("service")

	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)

	entityID, _ := body["entity_id"].(string)
	data, _ := body["data"].(map[string]any)

	s.bus.Publish("service.call", map[string]any{
		"service": domain + "." + service,
		"entity":  entityID,
		"data":    data,
	})

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	s.mu.Lock()
	s.clients[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
	}()

	// Send current state snapshot on connect.
	snapshot, _ := json.Marshal(map[string]any{
		"type":     "snapshot",
		"entities": s.store.All(),
	})
	conn.WriteMessage(websocket.TextMessage, snapshot)

	// Keep alive — read loop (discards client messages for now).
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

func (s *Server) broadcast(msg []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for conn := range s.clients {
		conn.WriteMessage(websocket.TextMessage, msg)
	}
}
