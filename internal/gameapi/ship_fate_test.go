package gameapi

import (
	"encoding/json"
	"testing"
	"time"
)

func TestShipDetailDecodesFateAndDatedFormerNames(t *testing.T) {
	for _, fate := range []string{"lost_at_sea", "captured", "seized_smuggling", "seized_bailiff", "seized_bankruptcy", "broken_up", "royal_navy"} {
		t.Run(fate, func(t *testing.T) {
			raw := []byte(`{"ship":{"id":"hull","status":"sunk","fate":"` + fate + `"},"former_names":[{"name":"Lapras","renamed_at":"2026-11-01T10:00:00Z"},{"name":"Endeavour","renamed_at":"2026-10-31T09:00:00Z"}]}`)
			var detail ShipDetail
			if err := json.Unmarshal(raw, &detail); err != nil {
				t.Fatal(err)
			}
			if detail.Ship.Fate == nil || *detail.Ship.Fate != fate || len(detail.FormerNames) != 2 ||
				detail.FormerNames[0].Name != "Lapras" || !detail.FormerNames[0].RenamedAt.Equal(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)) ||
				!detail.FormerNames[0].RenamedAt.After(detail.FormerNames[1].RenamedAt) {
				t.Fatalf("fleet history was not decoded in newest-first order: %+v", detail)
			}
			encoded, err := json.Marshal(detail)
			if err != nil {
				t.Fatal(err)
			}
			var restored ShipDetail
			if err := json.Unmarshal(encoded, &restored); err != nil || restored.Ship.Fate == nil || *restored.Ship.Fate != fate || len(restored.FormerNames) != 2 {
				t.Fatalf("fleet payload lost its fate or name history: %s, %v", encoded, err)
			}
		})
	}
}

func TestShipDetailKeepsAfloatAndLegacyFatesAbsent(t *testing.T) {
	for _, raw := range []string{`{"ship":{"status":"sailing","fate":null},"former_names":[]}`, `{"ship":{"status":"sunk"}}`} {
		var detail ShipDetail
		if err := json.Unmarshal([]byte(raw), &detail); err != nil || detail.Ship.Fate != nil || len(detail.FormerNames) != 0 {
			t.Fatalf("absent fate or history acquired an invented value: %+v, %v", detail, err)
		}
	}
}
