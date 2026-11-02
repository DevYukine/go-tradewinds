package names

import "testing"

func TestExhaustSpeciesBeforeSuffixes(t *testing.T) {
	used := append([]string(nil), pokemon...)
	missing := len(pokemon) / 2
	want := used[missing]
	used = append(used[:missing], used[missing+1:]...)
	used = append(used, "Lapras 2")

	if got := Next(used); got != want {
		t.Fatalf("unused species skipped: got %q want %q", got, want)
	}
	used = append(used, want)
	if got := Next(used); got != pokemon[1]+" 2" {
		t.Fatalf("exhausted catalog reused a name: %q", got)
	}
}

func TestAssignedNamesIncludeOnlyCatalogAndGeneratedSuffixes(t *testing.T) {
	for _, name := range []string{"Lapras", "Mr. Mime", "Nidoran♀", "Lapras 2", "Mr. Mime 103"} {
		if !IsAssigned(name) {
			t.Errorf("generated catalog name %q was not recognized", name)
		}
	}
	for _, name := range []string{"Endeavour", "lapras", "Lapras 1", "Lapras 02", "Lapras -2", "Lapras 2 extra", ""} {
		if IsAssigned(name) {
			t.Errorf("foreign name %q was mistaken for an assigned name", name)
		}
	}
}
