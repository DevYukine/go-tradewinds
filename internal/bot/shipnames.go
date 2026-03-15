package bot

import (
	"fmt"
	"math/rand/v2"
	"strings"
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
	"Thordan's",
	"Nidhogg's",
	"Shinryu's",
	"Byakko's",
	"Suzaku's",
	"Seiryu's",
	"Titania's",
	"Innocence's",
	"Ruby Weapon's",
	"Emerald Weapon's",
	"Diamond Weapon's",
	"Endsinger's",
	"Barbariccia's",
	"Rubicante's",
	"Golbez's",
	"Zeromus'",
	"Valigarmanda's",
	"Zoraal Ja's",

	// Scions & Main Cast
	"Tataru's",
	"Y'shtola's",
	"Thancred's",
	"Urianger's",
	"G'raha's",
	"Emet-Selch's",
	"Estinien's",
	"Alphinaud's",
	"Alisaie's",
	"Minfilia's",
	"Krile's",
	"Papalymo's",
	"Lyse's",
	"Ryne's",
	"Venat's",
	"Hermes'",
	"Ardbert's",
	"Wuk Lamat's",
	"Zero's",

	// Ishgard
	"Haurchefant's",
	"Aymeric's",
	"Lucia's",
	"Ysayle's",
	"Hilda's",
	"Francel's",

	// Doma & Ala Mhigo
	"Hien's",
	"Yugiri's",
	"Gosetsu's",
	"Cirina's",
	"Sadu's",
	"Fordola's",

	// Villains
	"Zenos'",
	"Lahabrea's",
	"Elidibus'",
	"Gaius'",
	"Nero's",
	"Fandaniel's",
	"Ran'jit's",
	"Varis'",

	// Ancients & Elpis
	"Hythlodaeus'",
	"Meteion's",
	"Azem's",

	// NPCs & Friends
	"Hildibrand's",
	"Godbert's",
	"Nashu's",
	"Cid's",
	"Jessie's",
	"Gerolt's",
	"Rowena's",
	"Nanamo's",
	"Merlwyb's",
	"Kan-E-Senna's",
	"Raubahn's",
	"Lolorito's",
	"Dulia-Chai's",
	"Vrtra's",
	"Varshahn's",
	"Feo Ul's",
	"Giott's",
	"Lyna's",

	// Races & Peoples
	"Moogle's",
	"Tonberry's",
	"Namazu's",
	"Pixie's",
	"Amaro's",
	"Chocobo's",
	"Carbuncle's",
	"Mandragora's",
	"Cactuar's",
	"Bomb's",

	// Locations as possessive
	"Limsa's",
	"Ul'dah's",
	"Gridania's",
	"Ishgard's",
	"Kugane's",
	"Crystarium's",
	"Sharlayan's",
	"Radz-at-Han's",
	"Eulmore's",
	"Idyllshire's",
	"Revenant's",
	"Azys Lla's",
	"Elpis'",
	"Ultima Thule's",
	"Tuliyollal's",
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
	"Flagship",
	"Galley",
	"Corsair",
	"Brigantine",
	"Cog",

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
	"Savage Clear",
	"Ultimate Legend",
	"Prog Party",
	"PF Trap",
	"Blacklist Special",
	"One Chest Run",
	"Blue Mage Bus",
	"Unsynced Farm",
	"Fate Train",
	"Hunt Train",
	"S-Rank Express",
	"B-Rank Bumbler",
	"Eureka Explorer",
	"Bozja Runner",
	"Criterion Crawler",
	"Deep Dungeon Dive",
	"Palace Runner",
	"Heaven-on-High",
	"Orthos Descent",

	// Trait puns & FFXIV life
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
	"Aetheryte Taxi",
	"Teleport Debt",
	"Gil Laundering Co",
	"FC House Flipper",
	"Plot Camper",
	"Mogstation Special",
	"Fantasia Addict",
	"Glam is Endgame",
	"Triple Triad Ace",
	"Gold Saucer VIP",
	"Jumbo Cactpot Win",
	"Mini Cactpot Grind",
	"Chocobo Racer",
	"Ocean Fisher",
	"Island Sanctuary",
	"Venture Capitalist",
	"Spiritbond Express",
	"Desynth Special",
	"Pentameld Project",
	"Relic Grinder",
	"Zodiac Zephyr",
	"Anima Vessel",
	"Manderville Marvel",
	"Allagan Relic",
	"Resistance Vessel",
	"Magitek Cruiser",
}

// knownPrefixes is used to detect whether a ship already has an FFXIV name.
var knownPrefixes []string

func init() {
	knownPrefixes = make([]string, len(ffxivPrefixes))
	for i, p := range ffxivPrefixes {
		// Strip trailing possessive for matching: "Ifrit's" → "Ifrit"
		knownPrefixes[i] = strings.TrimRight(p, "'s")
	}
}

// GenerateShipName creates a random funny FFXIV-themed ship name.
func GenerateShipName() string {
	prefix := ffxivPrefixes[rand.IntN(len(ffxivPrefixes))]
	suffix := ffxivSuffixes[rand.IntN(len(ffxivSuffixes))]
	return fmt.Sprintf("%s %s", prefix, suffix)
}

// IsFFXIVName reports whether a ship name matches the FFXIV naming scheme
// (starts with a known character/primal prefix).
func IsFFXIVName(name string) bool {
	for _, prefix := range knownPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
