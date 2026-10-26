package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func competitionFixture() CompetitionInput {
	return CompetitionInput{
		AccountID: "one", At: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
		OwnCompanies: []string{"Our fleet", "Our other fleet"}, OwnCompaniesComplete: true,
		Ports: []CompetitionPortObservation{
			{ID: "a", Name: "Port A", Region: "north"},
			{ID: "b", Name: "Port B", Region: "north"},
			{ID: "remote", Name: "Remote", Region: "south"},
		},
	}
}

func TestAggregateCompetitionDeduplicatesShipsAndExcludesAllOwnCompanies(t *testing.T) {
	input := competitionFixture()
	remote := "remote"
	notice := gameapi.SailingNotice{
		PortID: "a", CompanyName: "Rival", ShipName: "Swift", Direction: "departed",
		SeenAt: input.At.Add(-time.Hour), OtherPortID: &remote,
	}
	later := notice
	later.PortID, later.Direction, later.SeenAt = "b", "arrived", input.At.Add(-time.Minute)
	other := notice
	other.CompanyName = "Another rival"
	own, ownOther := notice, notice
	own.CompanyName, ownOther.CompanyName = "Our fleet", "Our other fleet"
	input.Sailings = []gameapi.SailingNotice{notice, notice, later, other, own, ownOther}
	input.Ports[0].Sailings = []gameapi.SailingNotice{notice}
	result := AggregateCompetition(input)
	north, south := result.Regions[0], result.Regions[1]
	if *north.ForeignCompanies != 2 || *north.ForeignShips != 2 || north.Sightings != 3 || north.ObservedPorts != 2 {
		t.Fatalf("regional counts must union names, not sum ports: %+v", north)
	}
	if *north.Ports[0].ForeignShips != 2 || north.Ports[0].Sightings != 2 || len(north.Ports[0].Ships) != 2 {
		t.Fatalf("duplicate SSE/REST notice inflated port counts: %+v", north.Ports[0])
	}
	if north.TopCompetitors[0].Name != "Rival" || north.TopCompetitors[0].Ships != 1 || north.TopCompetitors[0].Sightings != 2 || !north.LastSeen.Equal(later.SeenAt) {
		t.Fatalf("top companies or last seen incorrect: %+v", north.CompetitionActivity)
	}
	if south.ForeignShips != nil || south.Coverage != "not_observed" || south.Ports[0].ForeignCompanies != nil {
		t.Fatalf("reported destination became a direct sighting: %+v", south)
	}
}

func TestAggregateCompetitionUsesInclusiveUpstreamWindow(t *testing.T) {
	input := competitionFixture()
	for i, at := range []time.Time{
		input.At.Add(-CompetitionWindow - time.Nanosecond), input.At.Add(-CompetitionWindow),
		input.At, input.At.Add(time.Nanosecond),
	} {
		input.Sailings = append(input.Sailings, gameapi.SailingNotice{
			PortID: "a", CompanyName: "Rival", ShipName: fmt.Sprintf("Ship %d", i), SeenAt: at,
		})
	}
	result := AggregateCompetition(input)
	port := result.Regions[0].Ports[0]
	if *port.ForeignShips != 2 || port.Sightings != 2 || !port.LastSeen.Equal(input.At) {
		t.Fatalf("window must include both boundaries and exclude future activity: %+v", port)
	}
}

func TestAggregateCompetitionDistinguishesUnknownFromObservedZero(t *testing.T) {
	input := competitionFixture()
	zero := int64(0)
	input.Ports[0].ObservedAt = input.At.Add(-time.Minute)
	input.Ports[0].QuayVisible = true
	input.Ports[0].PlayerVolumeTons = &zero
	input.Ports[1].ObservedAt = input.At.Add(-CompetitionWindow - time.Second)
	input.Ports[1].QuayVisible = true
	result := AggregateCompetition(input)
	observed, unknown := result.Regions[0].Ports[0], result.Regions[0].Ports[1]
	if observed.ForeignShips == nil || *observed.ForeignShips != 0 || observed.Coverage != "partial" || observed.PlayerVolumeTons == nil || *observed.PlayerVolumeTons != 0 {
		t.Fatalf("empty quay sample lost its observed zero: %+v", observed)
	}
	if unknown.ForeignShips != nil || unknown.Coverage != "not_observed" || unknown.PlayerVolumeTons != nil {
		t.Fatalf("old quay sample became current zero coverage: %+v", unknown)
	}
	input.OwnCompaniesComplete = false
	input.Sailings = []gameapi.SailingNotice{{PortID: "a", CompanyName: "Unresolved owner", ShipName: "Swift", SeenAt: input.At}}
	result = AggregateCompetition(input)
	if result.Regions[0].ForeignShips != nil || len(result.Regions[0].Ports[0].Ships) != 0 || result.OwnCompaniesComplete {
		t.Fatalf("missing ownership evidence must not label our ships foreign: %+v", result)
	}
}

func TestAggregateCompetitionRanksTopCompaniesByDistinctShips(t *testing.T) {
	input := competitionFixture()
	for company, ships := range map[string]int{"Alpha": 1, "Beta": 2, "Gamma": 3, "Delta": 4} {
		for ship := range ships {
			input.Sailings = append(input.Sailings, gameapi.SailingNotice{
				PortID: "a", CompanyName: company, ShipName: fmt.Sprint(ship), SeenAt: input.At,
			})
		}
	}
	top := AggregateCompetition(input).Regions[0].TopCompetitors
	if len(top) != 3 || top[0].Name != "Delta" || top[1].Name != "Gamma" || top[2].Name != "Beta" {
		t.Fatalf("unexpected top companies: %+v", top)
	}
}

func TestPostgresCompetitionIsAccountScopedAndUsesSourceTime(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	saveTestAccount(t, database, "two")
	at := time.Now().UTC().Add(time.Second)
	for id, name := range map[string]string{"one": "Our fleet", "two": "Our other fleet"} {
		profile, _ := json.Marshal(map[string]any{"company": map[string]string{"id": "company-" + id, "name": name}})
		if err := database.SaveSnapshot(ctx, id, map[string]json.RawMessage{
			"profile": profile,
			"catalog": json.RawMessage(`{"ports":[{"id":"a","name":"Port A","banking_region":"north"},{"id":"b","name":"Port B","banking_region":"north"}]}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i, test := range []struct {
		account, company, ship string
		seenAt                 time.Time
	}{
		{"one", "Rival", "Swift", at.Add(-time.Minute)},
		{"one", "Our fleet", "Home", at.Add(-time.Minute)},
		{"one", "Our other fleet", "Partner", at.Add(-time.Minute)},
		{"one", "Old rival", "Replay", at.Add(-25 * time.Hour)},
		{"two", "Other account only", "Hidden", at.Add(-time.Minute)},
	} {
		payload, _ := json.Marshal(map[string]any{
			"timestamp": test.seenAt, "payload": map[string]string{
				"port_id": "a", "company_name": test.company, "ship_name": test.ship, "direction": "departed",
			},
		})
		if err := database.RecordEvent(ctx, test.account, fmt.Sprint(i), "ship_sailed", payload); err != nil {
			t.Fatal(err)
		}
	}
	result, err := database.Competition(ctx, "one", at)
	if err != nil {
		t.Fatal(err)
	}
	region := result.Regions[0]
	if !result.OwnCompaniesComplete || *region.ForeignShips != 1 || region.TopCompetitors[0].Name != "Rival" || region.Ports[1].ForeignShips != nil {
		t.Fatalf("account scope, own exclusion or upstream replay filter failed: %+v", result)
	}
	if _, err := database.Competition(ctx, "missing", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if err := database.SaveSnapshot(ctx, "two", map[string]json.RawMessage{}); err != nil {
		t.Fatal(err)
	}
	result, err = database.Competition(ctx, "one", at)
	if err != nil || result.OwnCompaniesComplete || result.Regions[0].ForeignShips != nil {
		t.Fatalf("missing configured-company name must leave counts unknown: %+v, %v", result, err)
	}
}

func TestCompetitionPortSnapshotPreservesUnknownAndStaleSources(t *testing.T) {
	at := competitionFixture().At
	catalog := json.RawMessage(`{"ports":[{"id":"a","name":"Port A","banking_region":"north"},{"id":"b","name":"Port B","banking_region":"north"}]}`)
	ports := json.RawMessage(`{"a":{"visibility":"quay","recent_sailings":[],"rolling_player_volume_tons":0,"occupied_berths":0},"b":{"visibility":"none","recent_sailings":null}}`)
	metadata, _ := json.Marshal(map[string]any{"ports/a": map[string]any{"complete": true, "updated_at": at.Add(-3 * time.Minute)}})
	observations, err := competitionPorts(catalog, ports, metadata, at)
	if err != nil {
		t.Fatal(err)
	}
	if !observations[0].QuayVisible || !observations[0].Stale || observations[0].PlayerVolumeTons == nil || *observations[0].PlayerVolumeTons != 0 || observations[1].QuayVisible || observations[1].PlayerVolumeTons != nil {
		t.Fatalf("port snapshot fabricated availability or freshness: %+v", observations)
	}
}

func TestPostgresCompetitionBoundsEventsAndScrubsDisplayText(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, database, "one")
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"profile": json.RawMessage(`{"company":{"id":"company-one","name":"Our fleet"}}`),
		"catalog": json.RawMessage(`{"ports":[{"id":"a","name":"Port A","banking_region":"north"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{
		"timestamp": at.Add(-time.Minute),
		"payload": map[string]string{
			"port_id": "a", "company_name": "Rival", "ship_name": credentials.APIKey + " ship",
		},
	})
	events := make([]Event, maxCompetitionEvents+1)
	for i := range events {
		events[i] = Event{
			AccountID: "one", EventID: fmt.Sprint(i), EventType: "ship_sailed",
			Payload: payload, CreatedAt: at.Add(-time.Minute),
		}
	}
	if err := database.db.CreateInBatches(events, 500).Error; err != nil {
		t.Fatal(err)
	}
	result, err := database.Competition(ctx, "one", at)
	if err != nil {
		t.Fatal(err)
	}
	port := result.Regions[0].Ports[0]
	if !result.EventsTruncated || *port.ForeignShips != 1 || port.Sightings != 1 {
		t.Fatalf("bounded duplicate evidence inflated traffic or lost truncation: %+v", result)
	}
	if strings.Contains(port.Ships[0].ShipName, credentials.APIKey) || !strings.Contains(port.Ships[0].ShipName, redacted) {
		t.Fatalf("competition display did not scrub stored credentials")
	}
}
