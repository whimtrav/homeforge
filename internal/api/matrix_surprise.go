package api

// "Surprise me" — a server-side random scene composer. Every call returns a
// different but coherent themed scene assembled from the building blocks. Instant
// and free (no AI), so a kid can mash the button and keep getting new worlds.

import (
	"math/rand"
	"time"
)

func ri(rng *rand.Rand, lo, hi int) int { return lo + rng.Intn(hi-lo+1) }
func pick(rng *rand.Rand, o ...string) string { return o[rng.Intn(len(o))] }

func surpriseScene() *mScene {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	themes := []func(*rand.Rand) *mScene{
		surpriseCreeperChaos,
		surpriseDayVillage,
		surpriseSpookyNight,
		surpriseNether,
		surpriseSnowy,
		surpriseLavaCave,
		surpriseStarryHills,
		surpriseFarm,
	}
	return themes[rng.Intn(len(themes))](rng)
}

func surpriseCreeperChaos(rng *rand.Rand) *mScene {
	return &mScene{Name: "Creeper Chaos", Gamma: 2.5, Layers: []mLayer{
		{Type: "background", Kind: "gradient", Colors: []string{pick(rng, "#1a2a4a", "#2a1a3a", "#0a1a2a"), "#0a0f1a"}},
		{Type: "celestial", Kind: pick(rng, "sun", "moon"), Scale: 3, Speed: 1, Color: "#ffdd44"},
		{Type: "hills", Kind: "blocky", Y: ri(rng, 28, 34), Colors: []string{"#3a8a3a", "#5a3a1a"}},
		{Type: "trees", Count: ri(rng, 2, 4), Y: 31, Color: "#2e8b2e"},
		{Type: "creeper", Count: ri(rng, 2, 3), Y: ri(rng, 40, 43)},
		{Type: "tnt", Count: ri(rng, 1, 2), Y: 42},
	}}
}

func surpriseDayVillage(rng *rand.Rand) *mScene {
	return &mScene{Name: "Village Day", Gamma: 2.5, Layers: []mLayer{
		{Type: "background", Kind: "gradient", Colors: []string{"#2a5a9a", "#8fb8dd"}},
		{Type: "clouds", Count: ri(rng, 2, 4)},
		{Type: "celestial", Kind: "sun", Scale: 3, Speed: 1, Color: "#ffdd44"},
		{Type: "hills", Kind: pick(rng, "blocky", "rolling"), Y: ri(rng, 32, 36), Colors: []string{"#3a8a3a", "#5a3a1a"}},
		{Type: "houses", Count: ri(rng, 1, 3), Y: 33},
		{Type: "trees", Count: ri(rng, 2, 3), Y: 33, Color: "#2e8b2e"},
		{Type: "mob", Kind: "villager", Count: ri(rng, 1, 2), Y: 43},
		{Type: "animals", Kind: pick(rng, "sheep", "pig", "cow"), Count: ri(rng, 1, 2), Y: 44},
		{Type: "flowers", Count: ri(rng, 4, 8), Y: 44},
	}}
}

func surpriseSpookyNight(rng *rand.Rand) *mScene {
	return &mScene{Name: "Spooky Night", Gamma: 2.6, Layers: []mLayer{
		{Type: "background", Kind: "gradient", Colors: []string{"#0a0f22", "#03040a"}},
		{Type: "stars", Count: ri(rng, 40, 60), H: 30},
		{Type: "celestial", Kind: "moon", X: ri(rng, 30, 40), Y: ri(rng, 7, 12), Scale: 4, Color: "#e6eaf5"},
		{Type: "hills", Kind: "blocky", Y: ri(rng, 33, 37), Colors: []string{"#26402a", "#141d16"}},
		{Type: "torch", Count: ri(rng, 2, 3), Y: 40},
		{Type: "mob", Kind: pick(rng, "zombie", "skeleton"), Count: ri(rng, 1, 2), Y: 43},
		{Type: "mob", Kind: pick(rng, "spider", "enderman"), Count: 1, Y: 43},
	}}
}

func surpriseNether(rng *rand.Rand) *mScene {
	return &mScene{Name: "Nether Gate", Gamma: 2.5, Layers: []mLayer{
		{Type: "background", Kind: "gradient", Colors: []string{"#3a0a0a", "#120303"}},
		{Type: "terrain", Kind: "stone", Y: ri(rng, 34, 38), Colors: []string{"#6a2a2a", "#2a1010"}},
		{Type: "pool", Kind: "lava", Y: ri(rng, 40, 42), H: ri(rng, 6, 8)},
		{Type: "portal", X: ri(rng, 14, 26), Y: 22, W: 8, H: 14},
		{Type: "particles", Kind: "embers", Count: ri(rng, 20, 30), Speed: 1, Color: "#ff8c1e"},
		{Type: "creeper", Count: 1, Y: 40},
	}}
}

func surpriseSnowy(rng *rand.Rand) *mScene {
	return &mScene{Name: "Snowy Woods", Gamma: 2.4, Layers: []mLayer{
		{Type: "background", Kind: "gradient", Colors: []string{"#3a4a66", "#1a2233"}},
		{Type: "particles", Kind: "snow", Count: ri(rng, 40, 60), Speed: 1, Color: "#eef2ff"},
		{Type: "hills", Kind: "rolling", Y: ri(rng, 33, 37), Colors: []string{"#d8e2f0", "#9aa8be"}},
		{Type: "trees", Count: ri(rng, 3, 5), Y: 34, Color: "#2e6b3e"},
		{Type: "animals", Kind: "sheep", Count: ri(rng, 1, 2), Y: 44},
	}}
}

func surpriseLavaCave(rng *rand.Rand) *mScene {
	return &mScene{Name: "Lava Cave", Gamma: 2.6, Layers: []mLayer{
		{Type: "background", Kind: "solid", Color: "#0a0810"},
		{Type: "terrain", Kind: "stone", Y: ri(rng, 8, 12), Colors: []string{"#5a5a66", "#26262e"}},
		{Type: "pool", Kind: "lava", Y: ri(rng, 38, 40), H: ri(rng, 8, 10)},
		{Type: "particles", Kind: "embers", Count: ri(rng, 20, 28), Speed: 1, Color: "#ff8c1e"},
	}}
}

func surpriseStarryHills(rng *rand.Rand) *mScene {
	return &mScene{Name: "Starry Hills", Gamma: 2.5, Layers: []mLayer{
		{Type: "background", Kind: "gradient", Colors: []string{"#0a1030", "#02030c"}},
		{Type: "stars", Count: ri(rng, 45, 65), H: 32},
		{Type: "celestial", Kind: "moon", X: ri(rng, 30, 40), Y: ri(rng, 7, 12), Scale: 4, Color: "#e6eaf5"},
		{Type: "hills", Kind: pick(rng, "blocky", "rolling"), Y: ri(rng, 34, 38), Colors: []string{"#1e5a24", "#123018"}},
		{Type: "trees", Count: ri(rng, 2, 4), Y: 35, Color: "#2e7b3e"},
		{Type: "flowers", Count: ri(rng, 3, 6), Y: 44},
	}}
}

func surpriseFarm(rng *rand.Rand) *mScene {
	return &mScene{Name: "Sunny Farm", Gamma: 2.5, Layers: []mLayer{
		{Type: "background", Kind: "gradient", Colors: []string{"#3a6aaa", "#a8cfe8"}},
		{Type: "clouds", Count: ri(rng, 2, 4)},
		{Type: "celestial", Kind: "sun", Scale: 3, Speed: 1, Color: "#ffdd44"},
		{Type: "terrain", Kind: "grass", Y: ri(rng, 36, 40), Colors: []string{"#3a9a3a", "#2a6a2a"}},
		{Type: "animals", Kind: "cow", Count: ri(rng, 1, 2), Y: 44},
		{Type: "animals", Kind: "pig", Count: ri(rng, 1, 2), Y: 45},
		{Type: "animals", Kind: "sheep", Count: ri(rng, 1, 2), Y: 43},
		{Type: "flowers", Count: ri(rng, 5, 9), Y: 45},
	}}
}
