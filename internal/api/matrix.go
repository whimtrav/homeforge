package api

// D1 Matrix control plane. HomeForge drives Victor's 48x48 LED panel two ways:
//   - built-in scenes: an HTTP GET to the firmware's /set?scene=N (the panel
//     renders + persists it on-device, no streaming needed).
//   - custom scenes: HomeForge renders mScene frames here and streams them over
//     UDP :21324 at ~20 fps. The firmware holds a streamed frame for 2s
//     (STREAM_HOLD), so the engine goroutine must keep sending or the panel
//     reverts to its on-device scene.
// A custom scene is persisted to /data/matrix-scene.json and resumed on startup.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	matrixDefaultIP = "192.168.1.50"
	matrixStreamPort = 21324
	matrixStatePath  = "/data/matrix-scene.json"
)

// builtinScene names the on-device firmware scenes (index = /set?scene=N).
type builtinScene struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

var matrixBuiltins = []builtinScene{
	{0, "World"}, {1, "Creeper"}, {2, "Rainbow"}, {5, "Plasma"},
	{6, "Village"}, {7, "Pacman"}, {8, "Minecraft"}, {9, "Day"},
	{10, "Volcano"}, {11, "Cave"}, {3, "Solid"}, {4, "Off"},
}

type matrixController struct {
	ip         string
	streamAddr string

	usage *matrixUsage // per-user AI generation limits

	mu       sync.Mutex
	cancel   context.CancelFunc
	mode     string  // "device" | "custom"
	scene    int     // last built-in scene id set
	spec     *mScene // active custom scene (nil when device mode)
}

type matrixState struct {
	Mode  string  `json:"mode"`
	Scene int     `json:"scene"`
	Spec  *mScene `json:"spec,omitempty"`
}

func newMatrixController(ip string) *matrixController {
	if ip == "" {
		ip = matrixDefaultIP
	}
	m := &matrixController{
		ip:         ip,
		streamAddr: fmt.Sprintf("%s:%d", ip, matrixStreamPort),
		usage:      newMatrixUsage(),
		mode:       "device",
		scene:      11,
	}
	m.resume()
	return m
}

// ---- device HTTP control ----

func (m *matrixController) deviceSet(query string) (string, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	r, err := c.Get("http://" + m.ip + "/set?" + query)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return strings.TrimSpace(string(b)), nil
}

func (m *matrixController) reachable() bool {
	_, err := m.deviceSet("")
	return err == nil
}

// ---- streaming ----

// streamFrame sends one 48x48 frame as UDP packets matching the firmware:
// [ 'D','M', flags, 0, start(hi,lo), count(hi,lo), RGB... ]; flag bit0 on the
// last packet tells the panel to latch (FastLED.show()).
func streamFrame(conn net.Conn, px []byte) {
	const per = 480
	for start := 0; start < mNumLeds; start += per {
		count := per
		if start+count > mNumLeds {
			count = mNumLeds - start
		}
		last := start+count >= mNumLeds
		pkt := make([]byte, 8+count*3)
		pkt[0], pkt[1] = 'D', 'M'
		if last {
			pkt[2] = 1
		}
		pkt[4] = byte(start >> 8)
		pkt[5] = byte(start)
		pkt[6] = byte(count >> 8)
		pkt[7] = byte(count)
		copy(pkt[8:], px[start*3:(start+count)*3])
		_, _ = conn.Write(pkt)
	}
}

func (m *matrixController) stopEngine() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mu.Unlock()
}

// startCustom renders + streams a scene until replaced/stopped.
func (m *matrixController) startCustom(s *mScene) {
	m.stopEngine()
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancel = cancel
	m.mode = "custom"
	m.spec = s
	m.mu.Unlock()

	// A streamed scene implies the panel is on.
	_, _ = m.deviceSet("power=1")

	// Bake in the panel's crispness (streamed frames skip the on-device gamma).
	gamma := s.Gamma
	if gamma < 1.01 {
		gamma = 2.5
	}
	lut := gammaLUT(gamma)

	go func() {
		conn, err := net.Dial("udp", m.streamAddr)
		if err != nil {
			return
		}
		defer conn.Close()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		f := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				cv := renderScene(s, f)
				cv.applyLUT(&lut)
				streamFrame(conn, cv.bytes())
				f++
			}
		}
	}()
	m.save()
}

func (m *matrixController) setBuiltin(scene int, bri, gamma string) (string, error) {
	m.stopEngine()
	m.mu.Lock()
	m.scene = scene
	m.mode = "device"
	m.spec = nil
	m.mu.Unlock()
	q := "scene=" + strconv.Itoa(scene)
	if bri != "" {
		q += "&bri=" + bri
	}
	if gamma != "" {
		q += "&gamma=" + gamma
	}
	m.save()
	return m.deviceSet(q)
}

// startTest streams a full-brightness solid colour plus a sweeping white bar —
// an unmistakable bring-up check that UDP frames are reaching the panel.
func (m *matrixController) startTest(c rgb) {
	m.stopEngine()
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancel = cancel
	m.mode = "custom"
	m.spec = &mScene{Name: "test"}
	m.mu.Unlock()
	_, _ = m.deviceSet("power=1")
	go func() {
		conn, err := net.Dial("udp", m.streamAddr)
		if err != nil {
			return
		}
		defer conn.Close()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		f := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				cv := &mcanvas{}
				for y := 0; y < mHeight; y++ {
					for x := 0; x < mWidth; x++ {
						cv.set(x, y, c.r, c.g, c.b)
					}
				}
				bar := f % mWidth
				for y := 0; y < mHeight; y++ {
					cv.set(bar, y, 255, 255, 255)
				}
				streamFrame(conn, cv.bytes())
				f++
			}
		}
	}()
}

// ---- persistence ----

func (m *matrixController) save() {
	m.mu.Lock()
	st := matrixState{Mode: m.mode, Scene: m.scene, Spec: m.spec}
	m.mu.Unlock()
	data, _ := json.MarshalIndent(st, "", "  ")
	tmp := matrixStatePath + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, matrixStatePath)
	}
}

func (m *matrixController) resume() {
	data, err := os.ReadFile(matrixStatePath)
	if err != nil {
		return
	}
	var st matrixState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	if st.Mode == "custom" && st.Spec != nil && len(st.Spec.Layers) > 0 {
		m.startCustom(st.Spec)
	} else {
		m.mu.Lock()
		m.scene = st.Scene
		m.mu.Unlock()
	}
}

func (m *matrixController) statusJSON() map[string]any {
	m.mu.Lock()
	mode, scene := m.mode, m.scene
	var specName string
	if m.spec != nil {
		specName = m.spec.Name
	}
	m.mu.Unlock()
	return map[string]any{
		"reachable": m.reachable(),
		"mode":      mode,
		"scene":     scene,
		"spec":      specName,
		"ip":        m.ip,
	}
}

// ---- HTTP handlers (wired in server.go) ----

func (s *Server) handleMatrixStatus(w http.ResponseWriter, r *http.Request) {
	if s.matrix == nil {
		http.Error(w, "matrix not configured", http.StatusNotImplemented)
		return
	}
	writeJSON(w, s.matrix.statusJSON())
}

func (s *Server) handleMatrixScenes(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(builtinSpecs()))
	for k := range builtinSpecs() {
		names = append(names, k)
	}
	writeJSON(w, map[string]any{"builtin": matrixBuiltins, "custom": names})
}

func (s *Server) handleMatrixScene(w http.ResponseWriter, r *http.Request) {
	if s.matrix == nil {
		http.Error(w, "matrix not configured", http.StatusNotImplemented)
		return
	}
	var b struct {
		Scene int    `json:"scene"`
		Bri   string `json:"bri"`
		Gamma string `json:"gamma"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	out, err := s.matrix.setBuiltin(b.Scene, b.Bri, b.Gamma)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "device": out})
}

func (s *Server) handleMatrixApply(w http.ResponseWriter, r *http.Request) {
	if s.matrix == nil {
		http.Error(w, "matrix not configured", http.StatusNotImplemented)
		return
	}
	var b struct {
		Name string  `json:"name"`
		Spec *mScene `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	spec := b.Spec
	if spec == nil && b.Name != "" {
		spec = builtinSpecs()[b.Name]
	}
	if spec == nil || len(spec.Layers) == 0 {
		http.Error(w, "no scene", http.StatusBadRequest)
		return
	}
	s.matrix.startCustom(spec)
	writeJSON(w, map[string]any{"ok": true, "spec": spec.Name})
}

func (s *Server) handleMatrixTest(w http.ResponseWriter, r *http.Request) {
	if s.matrix == nil {
		http.Error(w, "matrix not configured", http.StatusNotImplemented)
		return
	}
	var b struct {
		Color string `json:"color"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	c := parseHex(b.Color)
	if b.Color == "" {
		c = rgb{255, 40, 40}
	}
	s.matrix.startTest(c)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleMatrixStop(w http.ResponseWriter, r *http.Request) {
	if s.matrix == nil {
		http.Error(w, "matrix not configured", http.StatusNotImplemented)
		return
	}
	s.matrix.stopEngine()
	s.matrix.mu.Lock()
	s.matrix.mode = "device"
	s.matrix.spec = nil
	s.matrix.mu.Unlock()
	s.matrix.save()
	writeJSON(w, map[string]any{"ok": true})
}

// handleMatrixSurprise builds + streams a random themed scene (no AI, instant).
func (s *Server) handleMatrixSurprise(w http.ResponseWriter, r *http.Request) {
	if s.matrix == nil {
		http.Error(w, "matrix not configured", http.StatusNotImplemented)
		return
	}
	sc := surpriseScene()
	s.matrix.startCustom(sc)
	email, ok := s.sessionEmail(r)
	if !ok || email == "" {
		email = "system"
	}
	logMatrixPrompt(email, "(surprise button)", map[string]any{"ok": true, "source": "surprise", "result": sc.Name})
	writeJSON(w, map[string]any{"ok": true, "result": sc.Name})
}
