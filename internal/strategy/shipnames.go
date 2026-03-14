package strategy

import (
	"fmt"
	"math/rand/v2"
)

// FFXIV-inspired ship names: primal puns, character references, and NPC nods.
var ffxivPrefixes = []string{
	// Primals / Trials
	"Ifrit's",
	"Titan's",
	"Garuda's",
	"Leviathan's",
	"Ramuh's",
	"Shiva's",
	"Bahamut's",
	"Odin's",
	"Alexander's",
	"Ravana's",
	"Bismarck's",
	"Sephirot's",
	"Zurvan's",
	"Susano's",
	"Lakshmi's",
	"Tsukuyomi's",
	"Hades'",
	"Hydaelyn's",
	"Zodiark's",

	// Characters / NPCs
	"Tataru's",
	"Y'shtola's",
	"Thancred's",
	"Urianger's",
	"G'raha's",
	"Emet-Selch's",
	"Estinien's",
	"Alphinaud's",
	"Alisaie's",
	"Haurchefant's",
	"Hildibrand's",
	"Godbert's",
	"Cid's",
	"Nero's",
	"Aymeric's",
	"Hien's",
	"Lyse's",
	"Minfilia's",
	"Krile's",
	"Papalymo's",
	"Yda's",
	"Zenos'",
	"Venat's",
	"Hermes'",
	"Lahabrea's",
	"Elidibus'",
}

var ffxivSuffixes = []string{
	// Nautical puns
	"Folly",
	"Fury",
	"Hubris",
	"Bargain",
	"Primal Barge",
	"Dreadnought",
	"Rowboat",
	"Raft of Wonders",

	// FFXIV references
	"Waking Sands Trip",
	"Duty Finder",
	"Limit Break",
	"Echo Chamber",
	"Aether Current",
	"Fantasia",
	"Tomestone",
	"Gil Sink",
	"Parse Padder",
	"Roulette Runner",
	"AFK Fisher",
	"Wiping Machine",
	"Tankbuster",
	"Enrage Timer",

	// Trait puns
	"Grand Enterprise",
	"Scion Express",
	"Crystal Hauler",
	"Eorzean Dream",
	"Market Board Flipper",
	"Retainer Bell",
	"Glamour Dresser",
	"Materia Melder",
	"Last Resort",
	"Copperbell Cruiser",
}

// generateShipName creates a random funny FFXIV-themed ship name.
func generateShipName() string {
	prefix := ffxivPrefixes[rand.IntN(len(ffxivPrefixes))]
	suffix := ffxivSuffixes[rand.IntN(len(ffxivSuffixes))]
	return fmt.Sprintf("%s %s", prefix, suffix)
}
