package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestAssessWarningsUpkeepSeizureNamesAndReason(t *testing.T) {
	for _, test := range []struct {
		fate   string
		reason string
	}{
		{"seized_bailiff", "Ship seized by the bailiff because upkeep went unpaid."},
		{"seized_bankruptcy", "Ship seized in bankruptcy because upkeep went unpaid."},
	} {
		t.Run(test.fate, func(t *testing.T) {
			input := warningsAcceptanceInput()
			ship := warningsAcceptanceShip("seized-ship-id", 2*time.Hour)
			ship.Ship.Name, ship.Ship.Status, ship.Ship.Fate = "Lapras", "sunk", &test.fate
			ship.Ship.UpdatedAt = warningsAcceptanceAt.Add(-time.Hour)
			input.Ships = []gameapi.ShipDetail{ship, warningsAcceptanceShip("Blastoise", 2*time.Hour)}
			warning := warningsAcceptanceRequire(t, input, "upkeep_seizure", true)
			if warning.Severity != "critical" || !warning.Since.Equal(ship.Ship.UpdatedAt) ||
				warning.Names["ship"] != "Lapras" || warning.Names["ship_id"] != "seized-ship-id" ||
				warning.Names["fate"] != test.fate || warning.Reason != test.reason {
				t.Fatalf("upkeep seizure lost its actual ship, timing or unpaid-upkeep reason: %+v", warning)
			}
			idle := warningsAcceptanceRequire(t, input, "fleet_idle", true)
			if idle.Facts["total"] != 1 || idle.Names["ship"] != "Blastoise" {
				t.Fatalf("seized hull remained in the active fleet: %+v", idle)
			}

			// Terminal evidence remains actionable when the controller is not executing.
			input.Engine = nil
			warningsAcceptanceRequire(t, input, "upkeep_seizure", true)
		})
	}
}

func TestAssessWarningsFatesDoNotInventLossStreaksOrUpkeepSeizures(t *testing.T) {
	for _, fate := range []*string{nil, new(""), new("lost_at_sea"), new("captured"), new("seized_smuggling"), new("broken_up"), new("royal_navy")} {
		name := "legacy"
		if fate != nil {
			name = "fate=" + *fate
		}
		t.Run(name, func(t *testing.T) {
			input := warningsAcceptanceInput()
			ship := warningsAcceptanceShip("Lapras", 2*time.Hour)
			ship.Ship.Status, ship.Ship.Fate = "sunk", fate
			input.Ships = []gameapi.ShipDetail{ship}
			warningsAcceptanceRequire(t, input, "upkeep_seizure", false)
			warningsAcceptanceRequire(t, input, "loss_streak", false)
			warningsAcceptanceRequire(t, input, "fleet_idle", false)
			input.Settlements = warningsAcceptanceLosses(2)
			warningsAcceptanceRequire(t, input, "loss_streak", false)
			input.Settlements = warningsAcceptanceLosses(3)
			warningsAcceptanceRequire(t, input, "loss_streak", true)
		})
	}
	input := warningsAcceptanceInput()
	ship := warningsAcceptanceShip("Lapras", 2*time.Hour)
	ship.Ship.Fate = new("seized_bailiff")
	input.Ships = []gameapi.ShipDetail{ship}
	warningsAcceptanceRequire(t, input, "upkeep_seizure", false)
}

func TestAssessWarningsUpkeepSeizureUsesLatestScrubbedName(t *testing.T) {
	input := warningsAcceptanceInput()
	ship := warningsAcceptanceShip("seized-ship-id", 2*time.Hour)
	ship.Ship.Name, ship.Ship.Status, ship.Ship.Fate = "Old name", "sunk", new("seized_bailiff")
	ship.Ship.BerthedAt = nil
	input.Ships = []gameapi.ShipDetail{ship}
	input.Metadata["ships"] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-time.Minute), Complete: true}
	detail := ship
	detail.Ship.Name = "Vaporeon secret-value"
	input.ShipDetails = map[string]gameapi.ShipDetail{ship.Ship.ID: detail}
	input.Metadata["ship_details/"+ship.Ship.ID] = WarningDataset{UpdatedAt: warningsAcceptanceAt, Complete: true}
	input.Secrets = []string{"secret-value"}
	warning := warningsAcceptanceRequire(t, input, "upkeep_seizure", true)
	if !strings.Contains(warning.Names["ship"], "Vaporeon") || strings.Contains(warning.Names["ship"], "secret-value") ||
		!strings.Contains(warning.Reason, "upkeep went unpaid") {
		t.Fatalf("seizure warning used a former name, exposed credentials or hid unpaid upkeep: %+v", warning)
	}
}

func TestAssessWarningsRetainsRecordedSeizureWithoutClaimingCurrentFleet(t *testing.T) {
	for _, test := range []struct {
		name string
		meta WarningDataset
	}{
		{name: "missing"},
		{name: "incomplete", meta: WarningDataset{UpdatedAt: warningsAcceptanceAt}},
		{name: "stale", meta: WarningDataset{Complete: true, UpdatedAt: warningsAcceptanceAt, Stale: true}},
		{name: "failed", meta: WarningDataset{Complete: true, UpdatedAt: warningsAcceptanceAt, Error: "refresh failed"}},
		{name: "future", meta: WarningDataset{Complete: true, UpdatedAt: warningsAcceptanceAt.Add(time.Second)}},
		{name: "old", meta: WarningDataset{Complete: true, UpdatedAt: warningsAcceptanceAt.Add(-WarningFreshFor)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := warningsAcceptanceInput()
			ship := warningsAcceptanceShip("Lapras", 2*time.Hour)
			ship.Ship.Status, ship.Ship.Fate = "sunk", new("seized_bailiff")
			input.Ships = []gameapi.ShipDetail{ship}
			input.Metadata["ships"] = test.meta
			assessment := AssessWarnings(input, warningsAcceptanceAt)
			if !slices.Contains(assessment.Suppressed, "upkeep_seizure") {
				t.Fatal("unassessed fleet was reported as checked for upkeep seizures")
			}
			if warningsAcceptanceFind(assessment.Warnings, "upkeep_seizure") == nil {
				t.Fatal("a failed refresh erased the recorded terminal seizure")
			}
			input.Ships = nil
			assessment = AssessWarnings(input, warningsAcceptanceAt)
			if !slices.Contains(assessment.Suppressed, "upkeep_seizure") ||
				warningsAcceptanceFind(assessment.Warnings, "upkeep_seizure") != nil {
				t.Fatal("missing current fleet was reported as an assessed absence of seizures")
			}
		})
	}
}
