// Package names generates stable, collision-free Pokemon names for game assets.
package names

import (
	_ "embed"
	"strconv"
	"strings"
)

const DefaultHandle = "Yukine"
const DefaultCompany = "The Pokemon Pirates"

// Catalog source: PokeAPI pokemon_species_names.csv, English species names.
// Lapras leads, then National Pokedex order. Forms do not consume new names.
//
//go:embed pokemon.txt
var catalog string

var pokemon = strings.FieldsFunc(catalog, func(r rune) bool {
	return r == '\n' || r == '\r'
})

// Next returns the first unused name. Numbered generations keep names unique
// after the vocabulary is exhausted, including names already used by sunk ships.
func Next(used []string) string {
	taken := make(map[string]struct{}, len(used))
	for _, name := range used {
		taken[name] = struct{}{}
	}
	for generation := 1; ; generation++ {
		for _, base := range pokemon {
			name := base
			if generation > 1 {
				name += " " + strconv.Itoa(generation)
			}
			if _, exists := taken[name]; !exists {
				return name
			}
		}
	}
}
