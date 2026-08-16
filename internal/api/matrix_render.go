package api

// D1 Matrix scene renderer. The panel is a 48x48 WS2812B grid in Victor's room
// (LOLIN ESP32 @ 192.168.1.50). Its firmware accepts raster-order RGB frames over
// UDP :21324 and shows them straight to the LEDs (no on-device gamma), so this
// renderer bakes in the panel's "clarity" rule itself: DARK negative space +
// bright, distinct subjects — lit fills are kept dim so the panel never blooms
// into haze. A scene is a small JSON DSL (mScene) the AI can emit; renderScene
// composites its layers into an animated frame (f = frame counter, ~20 fps).

import (
	"math"
	"strconv"
	"strings"
)

const (
	mWidth   = 48
	mHeight  = 48
	mNumLeds = mWidth * mHeight
)

// mcanvas is a 48x48 RGB frame in raster order (idx = y*48 + x).
type mcanvas struct {
	p [mNumLeds][3]uint8
}

func satAdd(a, b uint8) uint8 {
	s := uint16(a) + uint16(b)
	if s > 255 {
		return 255
	}
	return uint8(s)
}

func (c *mcanvas) set(x, y int, r, g, b uint8) {
	if x < 0 || x >= mWidth || y < 0 || y >= mHeight {
		return
	}
	c.p[y*mWidth+x] = [3]uint8{r, g, b}
}

func (c *mcanvas) add(x, y int, r, g, b uint8) {
	if x < 0 || x >= mWidth || y < 0 || y >= mHeight {
		return
	}
	i := y*mWidth + x
	c.p[i][0] = satAdd(c.p[i][0], r)
	c.p[i][1] = satAdd(c.p[i][1], g)
	c.p[i][2] = satAdd(c.p[i][2], b)
}

// bytes returns the frame as a flat RGB slice for UDP streaming.
func (c *mcanvas) bytes() []byte {
	out := make([]byte, mNumLeds*3)
	for i := 0; i < mNumLeds; i++ {
		out[i*3] = c.p[i][0]
		out[i*3+1] = c.p[i][1]
		out[i*3+2] = c.p[i][2]
	}
	return out
}

// ---- colors ----

type rgb struct{ r, g, b uint8 }

func parseHex(s string) rgb {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return rgb{}
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return rgb{}
	}
	return rgb{uint8(v >> 16), uint8(v >> 8), uint8(v)}
}

func (c rgb) scale(f float64) rgb {
	cl := func(v uint8) uint8 {
		x := float64(v) * f
		if x > 255 {
			return 255
		}
		if x < 0 {
			return 0
		}
		return uint8(x)
	}
	return rgb{cl(c.r), cl(c.g), cl(c.b)}
}

func lerp(a, b uint8, t float64) uint8 {
	return uint8(float64(a) + (float64(b)-float64(a))*t)
}
func blendRGB(a, b rgb, t float64) rgb {
	return rgb{lerp(a.r, b.r, t), lerp(a.g, b.g, t), lerp(a.b, b.b, t)}
}

// palette helpers with sane fallbacks so a sparse AI spec still renders.
func colAt(cs []string, i int, def rgb) rgb {
	if i < len(cs) && strings.TrimSpace(cs[i]) != "" {
		return parseHex(cs[i])
	}
	return def
}
func firstColor(l *mLayer, def rgb) rgb {
	if strings.TrimSpace(l.Color) != "" {
		return parseHex(l.Color)
	}
	if len(l.Colors) > 0 {
		return parseHex(l.Colors[0])
	}
	return def
}

// deterministic hash noise (stable per (a,b), no global RNG) for placing stars /
// particles / flicker without a persistent state.
func hash2(a, b int) uint32 {
	h := uint32(int64(a)*374761393 + int64(b)*668265263)
	h = (h ^ (h >> 13)) * 1274126177
	return h ^ (h >> 16)
}
func frand(a, b int) float64 { return float64(hash2(a, b)%10000) / 10000.0 }

// ---- scene DSL ----

// mLayer is one drawable in a scene. Only the fields relevant to Type are read,
// so the AI can emit a compact object. Colors are "#rrggbb".
type mLayer struct {
	Type   string   `json:"type"`
	Kind   string   `json:"kind,omitempty"`
	Colors []string `json:"colors,omitempty"`
	Color  string   `json:"color,omitempty"`
	X      int      `json:"x,omitempty"`
	Y      int      `json:"y,omitempty"`
	W      int      `json:"w,omitempty"`
	H      int      `json:"h,omitempty"`
	Count  int      `json:"count,omitempty"`
	Scale  int      `json:"scale,omitempty"`
	Speed  int      `json:"speed,omitempty"`
}

type mScene struct {
	Name   string   `json:"name"`
	Gamma  float64  `json:"gamma,omitempty"` // crispness; default 2.5 (matches the on-device pass)
	Layers []mLayer `json:"layers"`
}

// gammaLUT builds a 256-entry video-gamma table. This is the SAME crispness pass
// the firmware applies to its on-device scenes (napplyGamma_video, g_gamma=2.5) —
// but streamed frames bypass it on-device, so HomeForge must apply it here or
// dim/mid fills bloom into haze. "video" = a nonzero input never maps to 0.
func gammaLUT(g float64) [256]uint8 {
	var lut [256]uint8
	for i := 1; i < 256; i++ {
		v := math.Pow(float64(i)/255.0, g) * 255.0
		r := uint8(v)
		if r == 0 {
			r = 1
		}
		lut[i] = r
	}
	return lut
}

func (c *mcanvas) applyLUT(lut *[256]uint8) {
	for i := range c.p {
		c.p[i][0] = lut[c.p[i][0]]
		c.p[i][1] = lut[c.p[i][1]]
		c.p[i][2] = lut[c.p[i][2]]
	}
}

// renderScene composites all layers into a frame for frame counter f.
func renderScene(s *mScene, f int) *mcanvas {
	cv := &mcanvas{}
	for i := range s.Layers {
		drawLayer(cv, &s.Layers[i], f)
	}
	return cv
}

func drawLayer(cv *mcanvas, l *mLayer, f int) {
	switch strings.ToLower(l.Type) {
	case "background":
		drawBackground(cv, l)
	case "stars":
		drawStars(cv, l, f)
	case "celestial":
		drawCelestial(cv, l, f)
	case "terrain":
		drawTerrain(cv, l)
	case "hills":
		drawHills(cv, l, f)
	case "trees":
		drawTrees(cv, l)
	case "houses", "house", "village":
		drawHouses(cv, l)
	case "mob", "mobs", "zombie", "skeleton", "enderman", "spider", "steve", "player", "villager":
		drawMobs(cv, l, f)
	case "animals", "animal", "sheep", "pig", "cow":
		drawAnimals(cv, l, f)
	case "flowers", "flower":
		drawFlowers(cv, l)
	case "torch", "torches":
		drawTorches(cv, l, f)
	case "clouds", "cloud":
		drawClouds(cv, l, f)
	case "tnt":
		drawTNT(cv, l, f)
	case "portal":
		drawPortal(cv, l, f)
	case "creeper", "creepers":
		drawCreepers(cv, l, f)
	case "pool":
		drawPool(cv, l, f)
	case "particles":
		drawParticles(cv, l, f)
	}
}

// background: black (default) / solid / vertical gradient. Kept dim so it reads
// as backdrop, not a bright wash.
func drawBackground(cv *mcanvas, l *mLayer) {
	switch strings.ToLower(l.Kind) {
	case "solid":
		c := firstColor(l, rgb{6, 6, 10}).scale(0.55)
		for y := 0; y < mHeight; y++ {
			for x := 0; x < mWidth; x++ {
				cv.set(x, y, c.r, c.g, c.b)
			}
		}
	case "gradient":
		top := colAt(l.Colors, 0, rgb{10, 14, 40})
		bot := colAt(l.Colors, 1, rgb{2, 3, 12})
		for y := 0; y < mHeight; y++ {
			t := float64(y) / float64(mHeight-1)
			c := blendRGB(top, bot, t).scale(0.6)
			for x := 0; x < mWidth; x++ {
				cv.set(x, y, c.r, c.g, c.b)
			}
		}
	default: // "black" or unset: leave the canvas dark (best clarity)
	}
}

// stars: twinkling points in the sky region. Positions are hash-stable; only the
// bright ones at any instant are drawn, so the field sparkles against black.
func drawStars(cv *mcanvas, l *mLayer, f int) {
	n := l.Count
	if n <= 0 {
		n = 42
	}
	c := firstColor(l, rgb{210, 215, 240})
	region := l.H
	if region <= 0 {
		region = 30 // upper sky by default
	}
	for i := 0; i < n; i++ {
		sx := int(hash2(i, 1) % mWidth)
		sy := int(hash2(i, 2) % uint32(region))
		// twinkle: slow per-star sine, phase offset by star index
		tw := 0.5 + 0.5*math.Sin(float64(f)*0.12+frand(i, 7)*6.28)
		if tw < 0.55 {
			continue
		}
		s := (tw - 0.55) / 0.45
		cc := c.scale(s)
		cv.add(sx, sy, cc.r, cc.g, cc.b)
	}
}

// celestial: a sun or moon disc + soft glow at (x,y).
func drawCelestial(cv *mcanvas, l *mLayer, f int) {
	cx, cy := l.X, l.Y
	if cx == 0 && cy == 0 {
		cx, cy = 36, 10
	}
	if l.Speed > 0 {
		// full arc: rise on one side, cross the top, set on the other. The moon is
		// offset half a cycle so it follows / opposes the sun.
		period := 600
		off := 0.0
		if strings.ToLower(l.Kind) == "moon" {
			off = 0.5
		}
		ph := math.Mod(float64(f)/float64(period)*float64(l.Speed)+off, 1.0)
		cx = int(ph*60) - 6
		cy = 38 - int(math.Sin(ph*math.Pi)*30)
	}
	rad := l.Scale
	if rad <= 0 {
		rad = 4
	}
	var def rgb
	if strings.ToLower(l.Kind) == "moon" {
		def = rgb{225, 230, 245}
	} else {
		def = rgb{255, 220, 70}
	}
	c := firstColor(l, def)
	glow := c.scale(0.18)
	gr := rad + 3
	for dy := -gr; dy <= gr; dy++ {
		for dx := -gr; dx <= gr; dx++ {
			d2 := dx*dx + dy*dy
			if d2 <= rad*rad {
				cv.set(cx+dx, cy+dy, c.r, c.g, c.b)
			} else if d2 <= gr*gr {
				cv.add(cx+dx, cy+dy, glow.r, glow.g, glow.b)
			}
		}
	}
}

// terrain: fill from row Y to the bottom with a body colour; top rows get an
// accent (grass/sand/stone). A light dither breaks up the flatness.
func drawTerrain(cv *mcanvas, l *mLayer) {
	y0 := l.Y
	if y0 <= 0 {
		y0 = 34
	}
	top := colAt(l.Colors, 0, rgb{40, 150, 40})
	body := colAt(l.Colors, 1, rgb{80, 55, 30})
	if len(l.Colors) < 2 {
		if strings.ToLower(l.Kind) == "stone" {
			top, body = rgb{90, 90, 100}, rgb{45, 45, 55}
		} else if strings.ToLower(l.Kind) == "sand" {
			top, body = rgb{200, 175, 110}, rgb{150, 125, 80}
		}
	}
	for y := y0; y < mHeight; y++ {
		c := body
		if y < y0+2 {
			c = top
		}
		for x := 0; x < mWidth; x++ {
			cc := c
			if (x*7+y*13)&3 == 0 {
				cc = cc.scale(0.82)
			}
			cv.set(x, y, cc.r, cc.g, cc.b)
		}
	}
}

// pool: an animated rect of liquid. lava = dark red base + bright flicker;
// water = blue base + lighter shimmer.
func drawPool(cv *mcanvas, l *mLayer, f int) {
	x0, y0, w, h := l.X, l.Y, l.W, l.H
	if w <= 0 {
		w = mWidth
	}
	if h <= 0 {
		h = 8
	}
	if x0 == 0 && y0 == 0 && w == mWidth {
		y0 = mHeight - h
	}
	lava := strings.ToLower(l.Kind) != "water"
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			n := 0.5 + 0.5*math.Sin(float64(x)*0.6+float64(y)*0.5+float64(f)*0.25+frand(x, y)*6.28)
			var c rgb
			if lava {
				base := rgb{90, 18, 6}
				hot := rgb{255, 150, 30}
				c = blendRGB(base, hot, n*n)
			} else {
				base := rgb{8, 30, 90}
				sh := rgb{60, 130, 210}
				c = blendRGB(base, sh, n*0.7)
			}
			cv.set(x, y, c.r, c.g, c.b)
		}
	}
}

// particles: falling rain/snow or rising embers/steam. Positions come from a
// hash-stable column + a time offset so each particle streams smoothly.
func drawParticles(cv *mcanvas, l *mLayer, f int) {
	n := l.Count
	if n <= 0 {
		n = 40
	}
	sp := l.Speed
	if sp <= 0 {
		sp = 1
	}
	kind := strings.ToLower(l.Kind)
	for i := 0; i < n; i++ {
		x := int(hash2(i, 11) % mWidth)
		phase := int(hash2(i, 12) % mHeight)
		switch kind {
		case "snow":
			y := (phase + f*sp/2) % mHeight
			x2 := x + int(math.Sin(float64(f)*0.05+float64(i)))
			c := firstColor(l, rgb{230, 235, 245})
			cv.add(x2, y, c.r, c.g, c.b)
		case "embers":
			y := mHeight - 1 - ((phase + f*sp) % mHeight)
			fade := float64(y) / float64(mHeight)
			c := firstColor(l, rgb{255, 120, 30}).scale(0.4 + 0.6*(1-fade))
			cv.add(x, y, c.r, c.g, c.b)
		case "steam":
			y := mHeight - 1 - ((phase + f*sp/2) % mHeight)
			x2 := x + int(2*math.Sin(float64(f)*0.04+float64(i)))
			fade := float64(y) / float64(mHeight)
			c := firstColor(l, rgb{200, 205, 215}).scale(0.25 + 0.5*(1-fade))
			cv.add(x2, y, c.r, c.g, c.b)
		default: // rain
			y := (phase + f*sp*2) % mHeight
			c := firstColor(l, rgb{90, 140, 220})
			cv.add(x, y, c.r, c.g, c.b)
			cv.add(x, (y+1)%mHeight, c.scale(0.5).r, c.scale(0.5).g, c.scale(0.5).b)
		}
	}
}

func iabs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

// hills: a blocky (Minecraft) or rolling ground silhouette. Height varies across x
// from two low-frequency sines; "blocky" quantizes into 4px columns for the MC look.
func drawHills(cv *mcanvas, l *mLayer, f int) {
	baseY := l.Y
	if baseY <= 0 {
		baseY = 32
	}
	amp := l.H
	if amp <= 0 {
		amp = 6
	}
	top := colAt(l.Colors, 0, rgb{40, 150, 40})
	body := colAt(l.Colors, 1, rgb{80, 55, 30})
	step := 4
	if strings.ToLower(l.Kind) == "rolling" {
		step = 1
	}
	for bx := 0; bx < mWidth; bx += step {
		nx := float64(bx)
		n := 0.5*math.Sin(nx*0.18) + 0.5*math.Sin(nx*0.07+1.3)
		h := baseY - int(n*float64(amp))
		if h < 3 {
			h = 3
		}
		for dx := 0; dx < step && bx+dx < mWidth; dx++ {
			x := bx + dx
			for y := h; y < mHeight; y++ {
				c := body
				if y < h+2 {
					c = top
				}
				if (x*7+y*13)&3 == 0 {
					c = c.scale(0.82)
				}
				cv.set(x, y, c.r, c.g, c.b)
			}
		}
	}
}

// trees: a row of little trees on the ground (brown trunk + a leafy diamond canopy).
func drawTrees(cv *mcanvas, l *mLayer) {
	n := l.Count
	if n <= 0 {
		n = 3
	}
	gy := l.Y
	if gy <= 0 {
		gy = 34
	}
	canopy := firstColor(l, rgb{34, 139, 46})
	trunk := rgb{95, 58, 26}
	for i := 0; i < n; i++ {
		x := (i + 1) * mWidth / (n + 1)
		for ty := gy - 1; ty >= gy-3; ty-- {
			cv.set(x, ty, trunk.r, trunk.g, trunk.b)
		}
		cyc := gy - 6
		for dy := -3; dy <= 1; dy++ {
			for dx := -2; dx <= 2; dx++ {
				if iabs(dx)+iabs(dy) <= 3 {
					c := canopy
					if (x+dx+dy)&1 == 0 {
						c = c.scale(0.82)
					}
					cv.set(x+dx, cyc+dy, c.r, c.g, c.b)
				}
			}
		}
	}
}

// houses: a row of little houses (walls + pitched roof + door + windows).
func drawHouses(cv *mcanvas, l *mLayer) {
	n := l.Count
	if n <= 0 {
		n = 2
	}
	gy := l.Y
	if gy <= 0 {
		gy = 34
	}
	wall := firstColor(l, rgb{170, 130, 90})
	roof := rgb{150, 45, 35}
	for i := 0; i < n; i++ {
		x := (i + 1) * mWidth / (n + 1)
		houseAt(cv, x, gy, wall, roof)
	}
}

func houseAt(cv *mcanvas, x, gy int, wall, roof rgb) {
	const bw, bh = 7, 6
	for wy := 0; wy < bh; wy++ {
		for wx := 0; wx < bw; wx++ {
			cv.set(x-bw/2+wx, gy-1-wy, wall.r, wall.g, wall.b)
		}
	}
	cv.set(x, gy-1, 40, 26, 16) // door
	cv.set(x, gy-2, 40, 26, 16)
	cv.set(x-2, gy-4, 250, 220, 120) // windows
	cv.set(x+2, gy-4, 250, 220, 120)
	roofBase := gy - 1 - bh
	for ry := 0; ry < 4; ry++ {
		for rx := -(4 - ry); rx <= (4 - ry); rx++ {
			cv.set(x+rx, roofBase-ry, roof.r, roof.g, roof.b)
		}
	}
}

// creeperFace: the classic 8x8 creeper face (1 = dark face pixel).
var creeperFace = [8]uint8{0, 0b01100110, 0b01100110, 0b00011000, 0b00111100, 0b00111100, 0b00100100, 0}

func drawCreeperBody(cv *mcanvas, cx, feetY int, body rgb) {
	headTop := feetY - 12
	for ry := 0; ry < 8; ry++ {
		for rx := 0; rx < 8; rx++ {
			if (creeperFace[ry]>>(7-rx))&1 == 1 {
				cv.set(cx-4+rx, headTop+ry, 10, 12, 10)
			} else {
				cv.set(cx-4+rx, headTop+ry, body.r, body.g, body.b)
			}
		}
	}
	for ry := 0; ry < 4; ry++ {
		for rx := 0; rx < 4; rx++ {
			cv.set(cx-2+rx, headTop+8+ry, body.r, body.g, body.b)
		}
	}
}

// drawExplosion: an expanding burst, t in 0..1 (white-hot core → orange → fade).
func drawExplosion(cv *mcanvas, cx, cy int, t float64) {
	rad := t * 11
	for a := 0; a < 44; a++ {
		ang := float64(a) / 44.0 * 6.283
		r := rad * (0.65 + 0.35*float64((a*37)%10)/10.0)
		x := cx + int(math.Cos(ang)*r)
		y := cy + int(math.Sin(ang)*r)
		var c rgb
		if t < 0.4 {
			c = rgb{255, 245, 210}
		} else {
			fade := 1 - t
			c = rgb{uint8(255 * fade), uint8(140 * fade), uint8(20 * fade)}
		}
		cv.add(x, y, c.r, c.g, c.b)
	}
	if t < 0.5 {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				cv.add(cx+dx, cy+dy, 255, 240, 180)
			}
		}
	}
}

// creeper: N creepers on the ground that idle, flash white, then EXPLODE on a loop.
func drawCreepers(cv *mcanvas, l *mLayer, f int) {
	n := l.Count
	if n <= 0 {
		n = 2
	}
	gy := l.Y
	if gy <= 0 {
		gy = 40
	}
	const cycle = 120
	for i := 0; i < n; i++ {
		ph := (f + i*37) % cycle
		// wander left/right, but FREEZE at the moment it detonates so the blast +
		// crater stay put instead of drifting.
		wf := ph
		if wf > 66 {
			wf = 66
		}
		baseX := (i + 1) * mWidth / (n + 1)
		cx := baseX + int(8*math.Sin((float64(f-ph+wf)+float64(i)*50)*0.025))
		switch {
		case ph < 66:
			drawCreeperBody(cv, cx, gy, rgb{45, 165, 40})
		case ph < 74:
			if (ph/2)&1 == 0 {
				drawCreeperBody(cv, cx, gy, rgb{240, 250, 240})
			} else {
				drawCreeperBody(cv, cx, gy, rgb{120, 220, 90})
			}
		case ph < 88:
			drawExplosion(cv, cx, gy-6, float64(ph-74)/14.0)
		}
		// once it detonates it leaves a smoking crater in the ground until it respawns
		if ph >= 80 {
			craterAt(cv, cx, gy)
		}
	}
}

// craterAt carves a black bowl (with a charred rim) into the ground where a creeper
// blew up. It overwrites ground pixels drawn by an earlier hills/terrain layer; the
// hole "heals" on the next cycle when the ground layer redraws and no crater is cut.
func craterAt(cv *mcanvas, cx, gy int) {
	const rw = 6
	for dx := -rw; dx <= rw; dx++ {
		depth := 5 - (iabs(dx)*5)/rw
		for dy := -1; dy <= depth; dy++ {
			cv.set(cx+dx, gy+dy, 0, 0, 0)
		}
	}
	for dx := -rw - 1; dx <= rw+1; dx++ {
		cv.set(cx+dx, gy-1, 34, 20, 12) // charred rim
	}
}

// ---- game objects: mobs, animals, decor ----

// drawMobs: a row of Minecraft characters (kind from l.Kind or l.Type).
func drawMobs(cv *mcanvas, l *mLayer, f int) {
	kind := strings.ToLower(l.Kind)
	if kind == "" || kind == "mob" || kind == "mobs" {
		kind = strings.ToLower(l.Type)
	}
	n := l.Count
	if n <= 0 {
		n = 1
	}
	gy := l.Y
	if gy <= 0 {
		gy = 40
	}
	for i := 0; i < n; i++ {
		cx := l.X
		if l.X == 0 || n > 1 {
			cx = (i + 1) * mWidth / (n + 1)
		}
		cx += int(6 * math.Sin((float64(f)+float64(i)*40)*0.02))
		drawMob(cv, cx, gy, kind)
	}
}

func drawMob(cv *mcanvas, cx, fy int, kind string) {
	switch kind {
	case "enderman":
		for y := fy - 17; y < fy; y++ {
			for dx := -1; dx <= 1; dx++ {
				cv.set(cx+dx, y, 18, 12, 24)
			}
		}
		cv.set(cx-1, fy-15, 190, 90, 230)
		cv.set(cx+1, fy-15, 190, 90, 230)
		return
	case "spider":
		for dy := -2; dy <= 2; dy++ {
			for dx := -3; dx <= 3; dx++ {
				if dx*dx+dy*dy <= 10 {
					cv.set(cx+dx, fy-3+dy, 44, 30, 30)
				}
			}
		}
		for _, lx := range []int{-5, -4, 4, 5} {
			cv.set(cx+lx, fy-4, 30, 20, 20)
			cv.set(cx+lx, fy-2, 30, 20, 20)
		}
		cv.set(cx-1, fy-4, 230, 40, 40)
		cv.set(cx+1, fy-4, 230, 40, 40)
		return
	}
	var head, bodyc, legc rgb
	switch kind {
	case "zombie":
		head, bodyc, legc = rgb{74, 122, 58}, rgb{58, 90, 138}, rgb{40, 60, 40}
	case "skeleton":
		head, bodyc, legc = rgb{216, 216, 200}, rgb{200, 200, 186}, rgb{150, 150, 140}
	case "villager":
		head, bodyc, legc = rgb{176, 128, 80}, rgb{122, 90, 58}, rgb{60, 44, 30}
	default: // steve / player
		head, bodyc, legc = rgb{200, 149, 108}, rgb{42, 122, 200}, rgb{40, 40, 110}
	}
	for y := 0; y < 4; y++ {
		for x := -2; x < 2; x++ {
			cv.set(cx+x, fy-12+y, head.r, head.g, head.b)
		}
	}
	for y := 0; y < 5; y++ {
		for x := -2; x < 2; x++ {
			cv.set(cx+x, fy-8+y, bodyc.r, bodyc.g, bodyc.b)
		}
	}
	for y := 0; y < 3; y++ {
		cv.set(cx-1, fy-3+y, legc.r, legc.g, legc.b)
		cv.set(cx+1, fy-3+y, legc.r, legc.g, legc.b)
	}
}

// drawAnimals: sheep/pig/cow grazing along the ground.
func drawAnimals(cv *mcanvas, l *mLayer, f int) {
	kind := strings.ToLower(l.Kind)
	if kind == "" || kind == "animals" || kind == "animal" {
		kind = strings.ToLower(l.Type)
	}
	n := l.Count
	if n <= 0 {
		n = 2
	}
	gy := l.Y
	if gy <= 0 {
		gy = 40
	}
	for i := 0; i < n; i++ {
		cx := (i+1)*mWidth/(n+1) + int(5*math.Sin((float64(f)+float64(i)*60)*0.02))
		var body rgb
		switch kind {
		case "pig":
			body = rgb{225, 150, 165}
		case "cow":
			body = rgb{225, 225, 225}
		default:
			body = rgb{235, 235, 240}
		}
		for y := 0; y < 3; y++ {
			for x := -3; x < 3; x++ {
				cv.set(cx+x, gy-4+y, body.r, body.g, body.b)
			}
		}
		cv.set(cx+3, gy-4, body.r, body.g, body.b)
		cv.set(cx+3, gy-3, body.r, body.g, body.b)
		cv.set(cx-2, gy-1, 60, 50, 45)
		cv.set(cx+1, gy-1, 60, 50, 45)
		if kind == "cow" {
			cv.set(cx-2, gy-3, 20, 20, 20)
			cv.set(cx+1, gy-4, 20, 20, 20)
		}
		if kind == "pig" {
			cv.set(cx+3, gy-3, 200, 110, 130)
		}
	}
}

// drawFlowers: colourful flowers scattered on the ground.
func drawFlowers(cv *mcanvas, l *mLayer) {
	n := l.Count
	if n <= 0 {
		n = 6
	}
	gy := l.Y
	if gy <= 0 {
		gy = 42
	}
	pal := []rgb{{230, 60, 60}, {235, 210, 70}, {210, 90, 200}, {90, 140, 235}, {245, 130, 60}}
	for i := 0; i < n; i++ {
		x := int(hash2(i, 5) % mWidth)
		c := pal[i%len(pal)]
		cv.set(x, gy-1, 40, 120, 40)
		cv.set(x, gy-2, c.r, c.g, c.b)
		cv.set(x-1, gy-2, c.r, c.g, c.b)
		cv.set(x+1, gy-2, c.r, c.g, c.b)
		cv.set(x, gy-3, c.r, c.g, c.b)
	}
}

// drawTorches: flickering torches on the ground.
func drawTorches(cv *mcanvas, l *mLayer, f int) {
	n := l.Count
	if n <= 0 {
		n = 3
	}
	gy := l.Y
	if gy <= 0 {
		gy = 40
	}
	for i := 0; i < n; i++ {
		x := (i + 1) * mWidth / (n + 1)
		for ty := 0; ty < 3; ty++ {
			cv.set(x, gy-1-ty, 110, 70, 30)
		}
		fl := 0.6 + 0.4*math.Sin(float64(f)*0.4+float64(i))
		cv.set(x, gy-4, uint8(255*fl), uint8(200*fl), 20)
		cv.add(x-1, gy-4, uint8(120*fl), uint8(60*fl), 0)
		cv.add(x+1, gy-4, uint8(120*fl), uint8(60*fl), 0)
		cv.add(x, gy-5, uint8(180*fl), uint8(90*fl), 0)
	}
}

// drawClouds: white clouds drifting across the sky.
func drawClouds(cv *mcanvas, l *mLayer, f int) {
	n := l.Count
	if n <= 0 {
		n = 3
	}
	for c := 0; c < n; c++ {
		base := (f/8+c*23)%(mWidth+24) - 12
		yy := 5 + c*6
		for dx := 0; dx < 12; dx++ {
			for dy := 0; dy < 4; dy++ {
				if (dy == 1 || dy == 2) || (dx > 2 && dx < 10) {
					cv.set(base+dx, yy+dy, 205, 210, 222)
				}
			}
		}
	}
}

// drawTNT: red TNT blocks with a blinking fuse.
func drawTNT(cv *mcanvas, l *mLayer, f int) {
	n := l.Count
	if n <= 0 {
		n = 2
	}
	gy := l.Y
	if gy <= 0 {
		gy = 40
	}
	blink := (f/6)&1 == 0
	for i := 0; i < n; i++ {
		x := (i + 1) * mWidth / (n + 1)
		for dy := 0; dy < 6; dy++ {
			for dx := -3; dx < 3; dx++ {
				c := rgb{200, 40, 30}
				if dy == 2 || dy == 3 {
					c = rgb{40, 30, 30}
				}
				cv.set(x+dx, gy-1-dy, c.r, c.g, c.b)
			}
		}
		if blink {
			cv.set(x, gy-8, 255, 240, 120)
		}
	}
}

// drawPortal: a swirling purple nether/end portal.
func drawPortal(cv *mcanvas, l *mLayer, f int) {
	x0, y0, w, h := l.X, l.Y, l.W, l.H
	if w <= 0 {
		w = 8
	}
	if h <= 0 {
		h = 14
	}
	if x0 == 0 && y0 == 0 {
		x0, y0 = mWidth/2-w/2, mHeight-h-4
	}
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			n := 0.5 + 0.5*math.Sin(float64(x)*0.7+float64(y)*0.5+float64(f)*0.2)
			c := blendRGB(rgb{60, 10, 90}, rgb{190, 90, 230}, n)
			cv.set(x, y, c.r, c.g, c.b)
		}
	}
}

// ---- built-in example specs (also used by "surprise me" until the AI is wired) ----

func builtinSpecs() map[string]*mScene {
	return map[string]*mScene{
		"lava_cave": {
			Name: "Lava Cave",
			Layers: []mLayer{
				{Type: "background", Kind: "solid", Color: "#0a0810"},
				{Type: "terrain", Kind: "stone", Y: 8, Colors: []string{"#5a5a66", "#26262e"}},
				{Type: "pool", Kind: "lava", Y: 38, H: 10},
				{Type: "particles", Kind: "embers", Count: 24, Speed: 1, Color: "#ff8c1e"},
			},
		},
		"starry_night": {
			Name: "Starry Night",
			Layers: []mLayer{
				{Type: "background", Kind: "gradient", Colors: []string{"#0a1030", "#02030c"}},
				{Type: "stars", Count: 55, H: 34, Color: "#d2d7f0"},
				{Type: "celestial", Kind: "moon", X: 37, Y: 9, Scale: 4, Color: "#e6eaf5"},
				{Type: "terrain", Kind: "grass", Y: 40, Colors: []string{"#1e5a24", "#123018"}},
			},
		},
		"rain_night": {
			Name: "Rainy Night",
			Layers: []mLayer{
				{Type: "background", Kind: "gradient", Colors: []string{"#0a1424", "#04070f"}},
				{Type: "particles", Kind: "rain", Count: 55, Speed: 2, Color: "#5f8cdc"},
				{Type: "terrain", Kind: "stone", Y: 42, Colors: []string{"#2a3340", "#161c26"}},
			},
		},
	}
}
